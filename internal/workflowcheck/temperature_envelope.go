package workflowcheck

import (
	"fmt"
	"net/http/httptest"
	"os"
	"time"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/clock"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/httpapi"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/service"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/storage"
)

// temperatureEnvelope exercises one invariant across reading entry, restart
// recovery, detail reads, and completion: "ever exceeded the envelope" is a
// durable fact, while "still outside the envelope" follows the last accepted
// reading and gates completion.
func temperatureEnvelope(api *client) error {
	dataDir, err := os.MkdirTemp("", "cellar-envelope-check-*")
	if err != nil {
		return fmt.Errorf("create envelope check directory: %w", err)
	}
	defer os.RemoveAll(dataDir)

	// The supplied client backs a throwaway server; restart checks need a
	// snapshot directory that survives, so drive a dedicated server instead.
	store, err := storage.Open(dataDir)
	if err != nil {
		return err
	}
	server := httptest.NewServer(httpapi.NewHandler(service.New(store, clock.System{})))
	defer server.Close()
	api = newClient(server.URL)

	formula, err := ensureApprovedFormula(api, "Envelope Formula")
	if err != nil {
		return err
	}
	vesselA, err := createCheckVessel(api, "VSL-301", 300)
	if err != nil {
		return err
	}
	vesselB, err := createCheckVessel(api, "VSL-302", 300)
	if err != nil {
		return err
	}
	vesselC, err := createCheckVessel(api, "VSL-303", 300)
	if err != nil {
		return err
	}
	runA, err := createStartedRun(api, formula.ID, vesselA.ID, "RUN-ENVELOPE-A")
	if err != nil {
		return err
	}
	runB, err := createStartedRun(api, formula.ID, vesselB.ID, "RUN-ENVELOPE-B")
	if err != nil {
		return err
	}
	runC, err := createStartedRun(api, formula.ID, vesselC.ID, "RUN-ENVELOPE-C")
	if err != nil {
		return err
	}

	base := time.Now().UTC().Add(10 * time.Second)

	// Run A: an accepted reading exceeds the formula limit, then later accepted
	// readings return inside it. The current marker clears; the excursion latch
	// stays.
	if _, err := appendObservation(api, runA.Run.ID, 1, 1.050, 20.0, base.Add(10*time.Second)); err != nil {
		return err
	}
	breached, err := appendObservation(api, runA.Run.ID, 2, 1.030, 23.5, base.Add(20*time.Second))
	if err != nil {
		return err
	}
	if breached.Envelope == nil || breached.Envelope.TemperatureC != 23.5 ||
		breached.Excursion == nil || breached.Excursion.Sequence != 2 ||
		breached.Excursion.TemperatureC != 23.5 || len(breached.Observations) != 2 {
		return errInvalidWorkflow("accepted excursion was not marked as currently and historically outside the envelope")
	}
	recovered, err := appendObservation(api, runA.Run.ID, 3, 1.014, 20.1, base.Add(30*time.Second))
	if err != nil {
		return err
	}
	if recovered.Envelope != nil {
		return errInvalidWorkflow("returning inside the envelope did not clear the current marker")
	}
	if recovered.Excursion == nil || recovered.Excursion.Sequence != 2 {
		return errInvalidWorkflow("recovering inside the envelope erased the excursion history")
	}
	if len(recovered.Observations) != 3 {
		return errInvalidWorkflow("recovered run lost accepted readings")
	}

	// Run B: an impossible 80 C reading is rejected per the existing range
	// semantics, but the excursion still latches. Nothing else changes: no
	// reading in the series, no version bump.
	if _, err := appendObservation(api, runB.Run.ID, 1, 1.050, 20.0, base.Add(10*time.Second)); err != nil {
		return err
	}
	impossible, err := api.call("POST", "/v1/runs/"+runB.Run.ID+"/observations",
		map[string]any{
			"sequence":      2,
			"gravity":       1.030,
			"temperature_c": 80.0,
			"observed_at":   base.Add(20 * time.Second),
			"note":          "impossible entry",
		}, nil)
	if err != nil {
		return err
	}
	if err := requireCode(impossible, 400, string(domain.CodeInvalidInput),
		"reject impossible 80 Celsius reading"); err != nil {
		return err
	}
	runBState, err := readRun(api, runB.Run.ID)
	if err != nil {
		return err
	}
	if len(runBState.Observations) != 1 || runBState.Version != 4 {
		return errInvalidWorkflow("rejected impossible reading altered the reading series or version")
	}
	if runBState.Envelope != nil {
		return errInvalidWorkflow("rejected reading altered the current envelope marker")
	}
	if runBState.Excursion == nil || runBState.Excursion.Sequence != 2 ||
		runBState.Excursion.TemperatureC != 80.0 {
		return errInvalidWorkflow("rejected impossible reading did not latch the excursion fact")
	}
	second, err := appendObservation(api, runB.Run.ID, 2, 1.028, 19.9, base.Add(25*time.Second))
	if err != nil {
		return err
	}
	third, err := appendObservation(api, runB.Run.ID, 3, 1.014, 19.8, base.Add(35*time.Second))
	if err != nil {
		return err
	}
	if len(third.Observations) != 3 || second.Version != 5 || third.Version != 6 ||
		third.Envelope != nil || third.Excursion == nil ||
		third.Excursion.TemperatureC != 80.0 || third.Excursion.Sequence != 2 {
		return errInvalidWorkflow("accepted readings after a rejected excursion altered the wrong state")
	}

	// Run C: the last accepted reading is still outside the envelope, so
	// completion must be refused and the vessel must not start cleaning.
	if _, err := appendObservation(api, runC.Run.ID, 1, 1.050, 20.0, base.Add(10*time.Second)); err != nil {
		return err
	}
	stillOut, err := appendObservation(api, runC.Run.ID, 2, 1.030, 23.5, base.Add(20*time.Second))
	if err != nil {
		return err
	}
	conditioning, err := api.call("POST", "/v1/runs/"+runC.Run.ID+"/conditioning",
		nil, ptrInt64(stillOut.Version))
	if err != nil {
		return err
	}
	if err := requireStatus(conditioning, 200, "condition still-outside run"); err != nil {
		return err
	}
	var conditioned runView
	if err := conditioning.decode(&conditioned); err != nil {
		return err
	}
	blocked, err := api.call("POST", "/v1/runs/"+runC.Run.ID+"/complete",
		nil, ptrInt64(conditioned.Version))
	if err != nil {
		return err
	}
	if err := requireCode(blocked, 409, string(domain.CodeRunState),
		"complete run still outside the envelope"); err != nil {
		return err
	}
	runCState, err := readRun(api, runC.Run.ID)
	if err != nil {
		return err
	}
	if runCState.State != domain.RunConditioning || runCState.Metrics != nil ||
		runCState.Envelope == nil || runCState.Excursion == nil {
		return errInvalidWorkflow("blocked completion changed run state or markers")
	}
	if err := requireVesselState(readVessel(api, vesselC.ID), domain.VesselInUse); err != nil {
		return fmt.Errorf("blocked completion moved the vessel: %w", err)
	}

	// Simulate a controller restart over the same snapshot directory.
	server.Close()
	reopened, err := storage.Open(dataDir)
	if err != nil {
		return err
	}
	server = httptest.NewServer(httpapi.NewHandler(service.New(reopened, clock.System{})))
	defer server.Close()
	api = newClient(server.URL)

	recoveredA, err := readRun(api, runA.Run.ID)
	if err != nil {
		return err
	}
	if recoveredA.Envelope != nil || recoveredA.Excursion == nil ||
		recoveredA.Excursion.Sequence != 2 || len(recoveredA.Observations) != 3 {
		return errInvalidWorkflow("restart reconciled recovered run A incorrectly")
	}
	if err := completeRun(api, recoveredA, vesselA.ID, 3); err != nil {
		return err
	}

	recoveredB, err := readRun(api, runB.Run.ID)
	if err != nil {
		return err
	}
	if recoveredB.Envelope != nil || recoveredB.Excursion == nil ||
		recoveredB.Excursion.TemperatureC != 80.0 ||
		recoveredB.Excursion.Sequence != 2 || len(recoveredB.Observations) != 3 {
		return errInvalidWorkflow("restart reconciled latched-but-valid run B incorrectly")
	}
	if err := completeRun(api, recoveredB, vesselB.ID, 3); err != nil {
		return err
	}

	recoveredC, err := readRun(api, runC.Run.ID)
	if err != nil {
		return err
	}
	if recoveredC.Envelope == nil || recoveredC.Excursion == nil {
		return errInvalidWorkflow("restart lost the still-outside markers for run C")
	}
	stillBlocked, err := api.call("POST", "/v1/runs/"+runC.Run.ID+"/complete",
		nil, ptrInt64(recoveredC.Version))
	if err != nil {
		return err
	}
	if err := requireCode(stillBlocked, 409, string(domain.CodeRunState),
		"complete still-outside run after restart"); err != nil {
		return err
	}
	if err := requireVesselState(readVessel(api, vesselC.ID), domain.VesselInUse); err != nil {
		return fmt.Errorf("blocked completion after restart moved the vessel: %w", err)
	}
	return nil
}

func appendObservation(
	api *client,
	runID string,
	sequence int,
	gravity, temperature float64,
	at time.Time,
) (runView, error) {
	response, err := api.call("POST", "/v1/runs/"+runID+"/observations",
		map[string]any{
			"sequence":      sequence,
			"gravity":       gravity,
			"temperature_c": temperature,
			"observed_at":   at,
			"note":          "envelope check",
		}, nil)
	if err != nil {
		return runView{}, err
	}
	if err := requireStatus(response, 200, "append envelope observation"); err != nil {
		return runView{}, err
	}
	var run runView
	if err := response.decode(&run); err != nil {
		return runView{}, err
	}
	return run, nil
}

func readRun(api *client, runID string) (runView, error) {
	response, err := api.call("GET", "/v1/runs/"+runID, nil, nil)
	if err != nil {
		return runView{}, err
	}
	if err := requireStatus(response, 200, "read envelope run"); err != nil {
		return runView{}, err
	}
	var run runView
	if err := response.decode(&run); err != nil {
		return runView{}, err
	}
	return run, nil
}

func readVessel(api *client, vesselID string) vesselView {
	response, err := api.call("GET", "/v1/vessels/"+vesselID, nil, nil)
	if err != nil {
		return vesselView{}
	}
	var vessel vesselView
	_ = response.decode(&vessel)
	return vessel
}

// completeRun drives conditioning when needed and completion, then checks the
// completion metrics and vessel cleaning transition while the excursion latch
// remains visible on the completed run.
func completeRun(api *client, run runView, vesselID string, observationCount int) error {
	version := run.Version
	if run.State == domain.RunFermenting {
		response, err := api.call("POST", "/v1/runs/"+run.ID+"/conditioning",
			nil, ptrInt64(version))
		if err != nil {
			return err
		}
		if err := requireStatus(response, 200, "condition recovered run"); err != nil {
			return err
		}
		if err := response.decode(&run); err != nil {
			return err
		}
		version = run.Version
	}
	response, err := api.call("POST", "/v1/runs/"+run.ID+"/complete",
		nil, ptrInt64(version))
	if err != nil {
		return err
	}
	if err := requireStatus(response, 200, "complete recovered run"); err != nil {
		return err
	}
	var pair pairView
	if err := response.decode(&pair); err != nil {
		return err
	}
	if err := requireRunState(pair.Run, domain.RunCompleted); err != nil {
		return err
	}
	if err := requireVesselState(pair.Vessel, domain.VesselCleaning); err != nil {
		return err
	}
	if pair.Run.Metrics == nil ||
		pair.Run.Metrics.ObservationCount != observationCount ||
		pair.Run.Metrics.ApparentAttenuation <= 0 ||
		pair.Run.Metrics.EstimatedAlcoholABV <= 0 {
		return errInvalidWorkflow("completion metrics changed for recovered run")
	}
	if pair.Run.Excursion == nil {
		return errInvalidWorkflow("completion erased the excursion history")
	}
	readBack, err := readRun(api, run.ID)
	if err != nil {
		return err
	}
	if readBack.Excursion == nil || len(readBack.Observations) != observationCount {
		return errInvalidWorkflow("completed run lost the excursion record or readings")
	}
	return nil
}
