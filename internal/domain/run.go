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
	// Envelope tracks the run's current temperature-envelope position and is
	// rebuilt from the accepted readings; it is not persisted directly.
	Envelope *TemperatureBreach `json:"-"`
	// Breach is the sticky fact that the run has at some point left the
	// formula envelope. It survives recovery, invalid reports, and restarts.
	Breach    *TemperatureBreach `json:"temperature_breach,omitempty"`
	Metrics   *RunMetrics        `json:"metrics,omitempty"`
	Version   int64              `json:"version"`
	CreatedAt time.Time          `json:"created_at"`
	UpdatedAt time.Time          `json:"updated_at"`
}

// TemperatureBreach records a reading that left the attached formula's
// fermentation temperature envelope.
type TemperatureBreach struct {
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
	if r.Breach != nil {
		value := *r.Breach
		cloned.Breach = &value
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

// RecordBreach marks the sticky fact that the run has left the formula
// envelope. A later reading inside the envelope never clears it.
func (r *FermentationRun) RecordBreach(observation GravityObservation, limitC float64) {
	if WithinEnvelope(observation.TemperatureC, limitC) {
		return
	}
	r.Breach = NewTemperatureBreach(observation, limitC)
}

// SetCurrentEnvelope refreshes the run's current envelope position. A reading
// inside the envelope clears it; this derived marker is rebuilt on load.
func (r *FermentationRun) SetCurrentEnvelope(observation GravityObservation, limitC float64) {
	if WithinEnvelope(observation.TemperatureC, limitC) {
		r.Envelope = nil
		return
	}
	r.Envelope = NewTemperatureBreach(observation, limitC)
}

// HasTemperatureBreach reports whether the run has ever left its formula's
// fermentation temperature envelope. The fact is sticky and survives
// recovery.
func (r FermentationRun) HasTemperatureBreach() bool {
	return r.Breach != nil
}

// CurrentlyOutsideEnvelope reports whether the run's latest accepted reading
// still sits outside the formula's fermentation temperature envelope.
func (r FermentationRun) CurrentlyOutsideEnvelope() bool {
	return r.Envelope != nil
}

// NewTemperatureBreach builds a breach marker for a reading.
func NewTemperatureBreach(
	observation GravityObservation,
	limitC float64,
) *TemperatureBreach {
	return &TemperatureBreach{
		Sequence:     observation.Sequence,
		ObservedAt:   observation.ObservedAt.UTC(),
		TemperatureC: observation.TemperatureC,
		LimitC:       limitC,
	}
}

// CurrentEnvelopeFromReadings derives the run's current envelope position:
// non-nil only when the latest accepted reading is still outside the limit.
func CurrentEnvelopeFromReadings(
	readings []GravityObservation,
	limitC float64,
) *TemperatureBreach {
	if len(readings) == 0 {
		return nil
	}
	latest := readings[len(readings)-1]
	if WithinEnvelope(latest.TemperatureC, limitC) {
		return nil
	}
	return NewTemperatureBreach(latest, limitC)
}

// HistoricalBreachFromReadings derives the sticky envelope-breach fact from
// accepted readings: the last reading that was outside the limit.
func HistoricalBreachFromReadings(
	readings []GravityObservation,
	limitC float64,
) *TemperatureBreach {
	var breach *TemperatureBreach
	for _, observation := range readings {
		if WithinEnvelope(observation.TemperatureC, limitC) {
			continue
		}
		breach = NewTemperatureBreach(observation, limitC)
	}
	return breach
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
