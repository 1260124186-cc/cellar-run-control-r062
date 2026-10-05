package service

import (
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
)

func (s *Service) AppendObservation(
	id string,
	input ObservationInput,
) (domain.FermentationRun, error) {
	var updated domain.FermentationRun
	var rejected *domain.Error
	err := s.store.Execute(func(snapshot *domain.Snapshot) error {
		run, ok := snapshot.Runs[id]
		if !ok {
			return domain.NewError(domain.CodeRunNotFound, "run was not found")
		}
		now := s.clock.Now()
		observation := domain.GravityObservation{
			RunID:        run.ID,
			Sequence:     input.Sequence,
			Gravity:      input.Gravity,
			TemperatureC: input.TemperatureC,
			ObservedAt:   input.ObservedAt.UTC(),
			Note:         input.Note,
			ReceivedAt:   now,
		}
		envelopeLimit := envelopeLimitFor(snapshot, run)

		// Every reading goes through the full domain validation. Physically
		// valid readings beyond the formula envelope are accepted; they refresh
		// the current marker and latch the durable excursion.
		if addErr := run.AddObservation(observation); addErr != nil {
			// A rejected reading leaves no trace apart from the excursion fact:
			// even an impossible reading that exceeds the formula envelope must
			// keep the breach history visible without touching the reading
			// series, version, metrics, or vessel state. The latch transaction
			// is committed deliberately; the validation error is returned to
			// the caller afterwards. Other rejections roll back unchanged.
			if run.State == domain.RunFermenting && run.Excursion == nil &&
				!domain.WithinEnvelope(observation.TemperatureC, envelopeLimit) {
				run.MarkExcursion(observation, envelopeLimit)
				snapshot.Runs[id] = run.Clone()
				var ok bool
				rejected, ok = addErr.(*domain.Error)
				if !ok {
					return addErr
				}
				return nil
			}
			return addErr
		}
		run.ApplyEnvelope(observation, envelopeLimit)
		run.MarkExcursion(observation, envelopeLimit)
		snapshot.Runs[id] = run.Clone()
		updated = run.Clone()
		return nil
	})
	if err != nil {
		return domain.FermentationRun{}, err
	}
	if rejected != nil {
		return domain.FermentationRun{}, rejected
	}
	return updated, nil
}

// envelopeLimitFor resolves the fermentation temperature envelope that applies
// to a run through its attached formula.
func envelopeLimitFor(snapshot *domain.Snapshot, run domain.FermentationRun) float64 {
	formula, ok := snapshot.Formulas[run.FormulaID]
	if !ok {
		return 0
	}
	return formula.MaxFermentationTempC
}

func (s *Service) BeginConditioning(
	id string,
	input VersionInput,
) (domain.FermentationRun, error) {
	var updated domain.FermentationRun
	err := s.store.Execute(func(snapshot *domain.Snapshot) error {
		run, ok := snapshot.Runs[id]
		if !ok {
			return domain.NewError(domain.CodeRunNotFound, "run was not found")
		}
		if err := s.checkExpected(run.Version, input.ExpectedVersion); err != nil {
			return err
		}
		if err := run.BeginConditioning(s.clock.Now()); err != nil {
			return err
		}
		snapshot.Runs[id] = run.Clone()
		updated = run.Clone()
		return nil
	})
	if err != nil {
		return domain.FermentationRun{}, err
	}
	return updated, nil
}

func (s *Service) CompleteRun(
	id string,
	input VersionInput,
) (domain.FermentationRun, domain.Vessel, error) {
	var updatedRun domain.FermentationRun
	var updatedVessel domain.Vessel
	err := s.store.Execute(func(snapshot *domain.Snapshot) error {
		run, ok := snapshot.Runs[id]
		if !ok {
			return domain.NewError(domain.CodeRunNotFound, "run was not found")
		}
		if err := s.checkExpected(run.Version, input.ExpectedVersion); err != nil {
			return err
		}
		formula, ok := snapshot.Formulas[run.FormulaID]
		if !ok {
			return domain.NewError(domain.CodeFormulaNotFound, "attached formula was not found")
		}
		if run.HasEnvelopeBreach() {
			return domain.NewError(domain.CodeRunState,
				"run is still outside the formula fermentation temperature envelope")
		}
		if err := run.Complete(formula.TargetOriginalGravity, s.clock.Now()); err != nil {
			return err
		}
		if run.VesselID == nil {
			return domain.NewError(domain.CodeRunState, "run has no vessel")
		}
		vessel, ok := snapshot.Vessels[*run.VesselID]
		if !ok {
			return domain.NewError(domain.CodeVesselNotFound, "vessel was not found")
		}
		if err := vessel.StartCleaning(run.ID, s.clock.Now()); err != nil {
			return err
		}
		snapshot.Runs[id] = run.Clone()
		snapshot.Vessels[vessel.ID] = vessel
		updatedRun = run.Clone()
		updatedVessel = vessel
		return nil
	})
	if err != nil {
		return domain.FermentationRun{}, domain.Vessel{}, err
	}
	return updatedRun, updatedVessel, nil
}
