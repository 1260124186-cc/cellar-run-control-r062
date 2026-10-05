package service

import (
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
)

func (s *Service) AppendObservation(
	id string,
	input ObservationInput,
) (domain.FermentationRun, error) {
	var updated domain.FermentationRun
	var rejection *domain.Error
	err := s.store.Execute(func(snapshot *domain.Snapshot) error {
		run, ok := snapshot.Runs[id]
		if !ok {
			return domain.NewError(domain.CodeRunNotFound, "run was not found")
		}
		observation := domain.GravityObservation{
			RunID:        run.ID,
			Sequence:     input.Sequence,
			Gravity:      input.Gravity,
			TemperatureC: input.TemperatureC,
			ObservedAt:   input.ObservedAt.UTC(),
			Note:         input.Note,
			ReceivedAt:   s.clock.Now(),
		}
		envelopeLimit := envelopeLimitFor(snapshot, run)
		excursion := run.State == domain.RunFermenting &&
			!domain.WithinEnvelope(observation.TemperatureC, envelopeLimit)

		// Validate against a probe so a rejected report never changes the
		// observation sequence, metrics inputs, run version, or vessel state.
		probe := run.Clone()
		validationErr := probe.AddObservation(observation)
		if validationErr != nil {
			if !excursion {
				return validationErr
			}
			// The excursion fact is sticky even when the report itself is
			// rejected as impossible. Commit the marker without appending the
			// rejected reading, then surface the validation failure.
			run.RecordBreach(observation, envelopeLimit)
			snapshot.Runs[id] = run.Clone()
			updated = run.Clone()
			rejection = domain.AsError(validationErr)
			return nil
		}

		run = probe
		run.RecordBreach(observation, envelopeLimit)
		run.SetCurrentEnvelope(observation, envelopeLimit)
		snapshot.Runs[id] = run.Clone()
		updated = run.Clone()
		return nil
	})
	if err != nil {
		return domain.FermentationRun{}, err
	}
	if rejection != nil {
		return updated, rejection
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
		if run.CurrentlyOutsideEnvelope() {
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
