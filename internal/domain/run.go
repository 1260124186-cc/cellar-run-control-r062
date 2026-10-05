package domain

import "time"

type RunState string

const (
	RunPlanned      RunState = "planned"
	RunReserved     RunState = "reserved"
	RunFermenting   RunState = "fermenting"
	RunConditioning RunState = "conditioning"
	RunCompleted    RunState = "completed"
	RunAborted      RunState = "aborted"
)

type FermentationRun struct {
	ID             string               `json:"id"`
	Code           string               `json:"code"`
	FormulaID      string               `json:"formula_id"`
	VesselID       *string              `json:"vessel_id,omitempty"`
	PlannedVolumeL float64              `json:"planned_volume_liters"`
	Notes          string               `json:"notes,omitempty"`
	State          RunState             `json:"state"`
	StartedAt      *time.Time           `json:"started_at,omitempty"`
	EndedAt        *time.Time           `json:"ended_at,omitempty"`
	Observations   []GravityObservation `json:"observations"`
	// Envelope describes the most recently accepted reading relative to the
	// formula envelope. It is derived from the accepted observation series and
	// rebuilt from the readings when a snapshot is loaded.
	Envelope *TemperatureEnvelope `json:"temperature_envelope,omitempty"`
	// Excursion latches the first reading that ever left the formula envelope.
	// It is durable, survives recovery, and is never cleared by later in-range
	// readings or by the offending reading being rejected.
	Excursion *TemperatureEnvelope `json:"temperature_excursion,omitempty"`
	Metrics   *RunMetrics          `json:"metrics,omitempty"`
	Version   int64                `json:"version"`
	CreatedAt time.Time            `json:"created_at"`
	UpdatedAt time.Time            `json:"updated_at"`
}

// TemperatureEnvelope records a temperature reading relative to the attached
// formula's fermentation temperature envelope. It describes both the current
// position of the run (based on the last accepted reading) and the first
// reading that ever breached the envelope (the durable excursion latch).
type TemperatureEnvelope struct {
	Sequence     int       `json:"sequence"`
	ObservedAt   time.Time `json:"observed_at"`
	TemperatureC float64   `json:"temperature_c"`
	LimitC       float64   `json:"limit_c"`
}

func (r FermentationRun) Clone() FermentationRun {
	cloned := r
	cloned.Observations = append([]GravityObservation(nil), r.Observations...)
	if r.VesselID != nil {
		value := *r.VesselID
		cloned.VesselID = &value
	}
	if r.StartedAt != nil {
		value := *r.StartedAt
		cloned.StartedAt = &value
	}
	if r.EndedAt != nil {
		value := *r.EndedAt
		cloned.EndedAt = &value
	}
	if r.Metrics != nil {
		value := *r.Metrics
		cloned.Metrics = &value
	}
	if r.Envelope != nil {
		value := *r.Envelope
		cloned.Envelope = &value
	}
	if r.Excursion != nil {
		value := *r.Excursion
		cloned.Excursion = &value
	}
	return cloned
}

// WithinEnvelope reports whether a temperature reading stays inside the
// formula's maximum fermentation temperature.
func WithinEnvelope(temperatureC, limitC float64) bool {
	if limitC <= 0 {
		return true
	}
	return temperatureC <= limitC
}

// ApplyEnvelope refreshes the run's current envelope marker from the most
// recently accepted reading. A reading back inside the envelope clears the
// marker because the run is no longer sitting outside its temperature limits.
func (r *FermentationRun) ApplyEnvelope(observation GravityObservation, limitC float64) {
	if WithinEnvelope(observation.TemperatureC, limitC) {
		r.Envelope = nil
		return
	}
	r.Envelope = envelopeMarker(observation, limitC)
}

// MarkExcursion latches the durable record of a reading that left the formula
// envelope. The first excursion is retained permanently; later readings never
// overwrite or clear it, so the breach history stays visible even when the run
// returns to range or the reading is otherwise rejected.
func (r *FermentationRun) MarkExcursion(observation GravityObservation, limitC float64) {
	if WithinEnvelope(observation.TemperatureC, limitC) {
		return
	}
	if r.Excursion != nil {
		return
	}
	r.Excursion = envelopeMarker(observation, limitC)
}

func envelopeMarker(observation GravityObservation, limitC float64) *TemperatureEnvelope {
	return &TemperatureEnvelope{
		Sequence:     observation.Sequence,
		ObservedAt:   observation.ObservedAt.UTC(),
		TemperatureC: observation.TemperatureC,
		LimitC:       limitC,
	}
}

// HasEnvelopeBreach reports whether the run currently sits outside its
// formula's fermentation temperature envelope according to its last accepted
// reading.
func (r FermentationRun) HasEnvelopeBreach() bool {
	return r.Envelope != nil
}

// HadTemperatureExcursion reports whether any reading on the run ever left the
// formula envelope, even if the run has since returned inside it.
func (r FermentationRun) HadTemperatureExcursion() bool {
	return r.Excursion != nil
}

// CurrentEnvelopeFromReadings rebuilds the current envelope marker for a run
// loaded from a persisted snapshot. The marker follows the most recently
// accepted reading: it is present only when that reading is still outside the
// envelope, so runs that returned to range complete normally.
func CurrentEnvelopeFromReadings(
	readings []GravityObservation,
	limitC float64,
) *TemperatureEnvelope {
	if len(readings) == 0 {
		return nil
	}
	last := readings[len(readings)-1]
	if WithinEnvelope(last.TemperatureC, limitC) {
		return nil
	}
	return envelopeMarker(last, limitC)
}

// FirstExcursionFromReadings rebuilds the durable excursion latch for a run
// loaded from an older snapshot that does not carry the latch itself. The first
// reading that left the envelope wins.
func FirstExcursionFromReadings(
	readings []GravityObservation,
	limitC float64,
) *TemperatureEnvelope {
	for _, observation := range readings {
		if WithinEnvelope(observation.TemperatureC, limitC) {
			continue
		}
		return envelopeMarker(observation, limitC)
	}
	return nil
}

func (r FermentationRun) Validate() error {
	if err := ValidateRunCode(r.Code, "code"); err != nil {
		return err
	}
	if r.PlannedVolumeL <= 0 {
		return FieldError(CodeInvalidInput, "planned_volume_liters",
			"must be greater than zero")
	}
	if len([]rune(r.Notes)) > 1000 {
		return FieldError(CodeInvalidInput, "notes",
			"must not exceed 1000 characters")
	}
	return nil
}

func (r *FermentationRun) ReserveVessel(vesselID string, now time.Time) error {
	if r.State != RunPlanned {
		return NewError(CodeRunState, "only a planned run can reserve a vessel")
	}
	value := vesselID
	r.VesselID = &value
	r.State = RunReserved
	r.UpdatedAt = now.UTC()
	r.Version++
	return nil
}

func (r *FermentationRun) StartFermentation(now time.Time) error {
	if r.State != RunReserved || r.VesselID == nil {
		return NewError(CodeRunState, "only a reserved run can start fermentation")
	}
	stamp := now.UTC()
	r.State = RunFermenting
	r.StartedAt = &stamp
	r.UpdatedAt = stamp
	r.Version++
	return nil
}

func (r *FermentationRun) AddObservation(observation GravityObservation) error {
	if r.State != RunFermenting || r.StartedAt == nil {
		return NewError(CodeRunState, "observations require a fermenting run")
	}
	if len(r.Observations) >= 500 {
		return NewError(CodeObservationConflict,
			"run cannot contain more than 500 observations")
	}
	var previous *GravityObservation
	if len(r.Observations) > 0 {
		value := r.Observations[len(r.Observations)-1]
		previous = &value
	}
	if err := ValidateObservation(*r.StartedAt, previous, observation, observation.ReceivedAt); err != nil {
		return err
	}
	expected := len(r.Observations) + 1
	if observation.Sequence != expected {
		return NewError(CodeObservationConflict,
			"observation sequence must be contiguous and begin at 1")
	}
	observation.RunID = r.ID
	r.Observations = append(r.Observations, observation)
	r.UpdatedAt = observation.ReceivedAt.UTC()
	r.Version++
	return nil
}

func (r *FermentationRun) BeginConditioning(now time.Time) error {
	if r.State != RunFermenting {
		return NewError(CodeRunState, "only a fermenting run can enter conditioning")
	}
	if len(r.Observations) < 2 {
		return NewError(CodeRunState,
			"conditioning requires at least two observations")
	}
	r.State = RunConditioning
	r.UpdatedAt = now.UTC()
	r.Version++
	return nil
}

func (r *FermentationRun) Complete(originalGravity float64, now time.Time) error {
	if r.State != RunConditioning {
		return NewError(CodeRunState, "only a conditioning run can be completed")
	}
	if len(r.Observations) < 3 {
		return NewError(CodeRunState,
			"completion requires at least three observations")
	}
	stamp := now.UTC()
	final := r.Observations[len(r.Observations)-1]
	metrics := CalculateMetrics(originalGravity, final.Gravity, len(r.Observations))
	r.Metrics = &metrics
	r.State = RunCompleted
	r.EndedAt = &stamp
	r.UpdatedAt = stamp
	r.Version++
	return nil
}

func (r *FermentationRun) Abort(now time.Time) error {
	if r.State == RunCompleted || r.State == RunAborted {
		return NewError(CodeRunState, "completed or aborted runs cannot be aborted")
	}
	stamp := now.UTC()
	r.State = RunAborted
	r.EndedAt = &stamp
	r.UpdatedAt = stamp
	r.Version++
	return nil
}

func (r FermentationRun) HadStarted() bool {
	return r.StartedAt != nil
}
