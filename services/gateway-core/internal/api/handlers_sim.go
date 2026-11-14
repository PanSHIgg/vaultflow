package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"vaultflow/gateway-core/internal/ports"
	"vaultflow/gateway-core/internal/types"
)

// ---------------------------------------------------------------------------
// Dev simulator gate — 仅 ENABLE_DEV_SIMULATOR=true 时可用, 否则 404 FEATURE_DISABLED.
// ---------------------------------------------------------------------------

func (s *Server) simGate(w http.ResponseWriter, r *http.Request) bool {
	if s.deps.Config.SimEnabled {
		return true
	}
	writeError(w, r, notFoundCode("simulator is disabled"))
	return false
}

func notFoundCode(msg string) *APIError {
	return apiErr(http.StatusNotFound, CodeFeatureDisabled, msg)
}

// ---------------------------------------------------------------------------
// POST /v1/sim/anvil/{action} — simAnvil 裸调用透传
// ---------------------------------------------------------------------------

var simAnvilActions = map[string]bool{
	"snapshot": true, "mine": true, "revert": true, "drop-transaction": true,
}

func (s *Server) simAnvil(w http.ResponseWriter, r *http.Request) {
	if !s.simGate(w, r) {
		return
	}
	action := r.PathValue("action")
	if !simAnvilActions[action] {
		writeError(w, r, notFound("unknown anvil action"))
		return
	}
	var req struct {
		ChainID *int64         `json:"chain_id"`
		Params  map[string]any `json:"params"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if ae := decodeJSONBody(r, &req, s.deps.Config.MaxBodyBytes); ae != nil {
			writeError(w, r, ae)
			return
		}
	}
	if s.deps.Anvil == nil {
		writeError(w, r, upstream("anvil controller not wired"))
		return
	}
	var chainID uint64
	if req.ChainID != nil {
		if *req.ChainID <= 0 {
			writeError(w, r, badRequest("invalid chain_id"))
			return
		}
		chainID = uint64(*req.ChainID)
	}
	result, err := s.deps.Anvil.Invoke(r.Context(), chainID, action, req.Params)
	if err != nil {
		var ae *APIError
		if ae2, ok := err.(*APIError); ok {
			ae = ae2
		} else {
			ae = upstream("anvil call failed: " + err.Error())
		}
		writeError(w, r, ae)
		return
	}
	writeJSON(w, http.StatusOK, simAnvilResultDTO{Action: action, Result: result})
}

// ---------------------------------------------------------------------------
// POST /v1/sim/scenarios/{scenario} — runSimScenario
// ---------------------------------------------------------------------------

// openapi SimScenario 枚举 -> ports.SimScenario 映射.
// ⚠ ports 额外存在 INVARIANT_CHECK 场景, REST 面无对应枚举 (契约缺陷已上报).
var simScenarioMap = map[string]ports.SimScenario{
	"shallow-reorg":        ports.SimScenarioReorgShallow,
	"deep-reorg":           ports.SimScenarioReorgDeep,
	"blacklist-returnbomb": ports.SimScenarioQuarantineRevertGas,
	"signer-penetration":   ports.SimScenarioSignerPenetration,
}

var simScenarioName = map[ports.SimScenario]string{
	ports.SimScenarioReorgShallow:        "shallow-reorg",
	ports.SimScenarioReorgDeep:           "deep-reorg",
	ports.SimScenarioQuarantineRevertGas: "blacklist-returnbomb",
	ports.SimScenarioSignerPenetration:   "signer-penetration",
	ports.SimScenarioInvariantCheck:      "invariant-check",
}

func (s *Server) runSimScenario(w http.ResponseWriter, r *http.Request) {
	if !s.simGate(w, r) {
		return
	}
	scenario, ok := simScenarioMap[r.PathValue("scenario")]
	if !ok {
		writeError(w, r, notFound("unknown scenario"))
		return
	}
	var req struct {
		ChainID *int64         `json:"chain_id"`
		Options map[string]any `json:"options"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if ae := decodeJSONBody(r, &req, s.deps.Config.MaxBodyBytes); ae != nil {
			writeError(w, r, ae)
			return
		}
	}
	if s.deps.Sim == nil {
		writeError(w, r, upstream("simulator service not wired"))
		return
	}
	var chainID types.ChainID
	if req.ChainID != nil {
		if *req.ChainID <= 0 {
			writeError(w, r, badRequest("invalid chain_id"))
			return
		}
		chainID = types.ChainID(*req.ChainID)
	}
	params := map[string]string{}
	for k, v := range req.Options {
		switch tv := v.(type) {
		case string:
			params[k] = tv
		case float64:
			params[k] = jsonNumber(tv)
		case bool:
			params[k] = jsonBool(tv)
		default:
			raw, _ := json.Marshal(v)
			params[k] = string(raw)
		}
	}
	run, err := s.deps.Sim.Trigger(r.Context(), ports.SimTriggerRequest{
		Scenario: scenario, ChainID: chainID, Params: params,
	})
	if err != nil {
		writeError(w, r, mapError(err, ""))
		return
	}
	writeJSON(w, http.StatusAccepted, toSimRunDTO(run))
}

func jsonNumber(f float64) string {
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}
func jsonBool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// ---------------------------------------------------------------------------
// GET /v1/sim/runs/{runId} — getSimRun
// ---------------------------------------------------------------------------

func (s *Server) getSimRun(w http.ResponseWriter, r *http.Request) {
	if !s.simGate(w, r) {
		return
	}
	runID, ae := parsePathUUID(r, "runId")
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	if s.deps.Sim == nil {
		writeError(w, r, upstream("simulator service not wired"))
		return
	}
	run, err := s.deps.Sim.GetRun(r.Context(), runID)
	if err != nil {
		writeError(w, r, mapError(err, ""))
		return
	}
	writeJSON(w, http.StatusOK, toSimRunDTO(run))
}

// ---------------------------------------------------------------------------
// ports.SimRun -> openapi SimRun 投影
// ports.SimRunStatus: RUNNING/SUCCEEDED/FAILED; openapi: RUNNING/COMPLETED/FAILED
// (枚举名差异已上报, SUCCEEDED -> COMPLETED 映射).
// ---------------------------------------------------------------------------

func toSimRunDTO(run ports.SimRun) simRunDTO {
	dto := simRunDTO{
		RunID:      run.ID.String(),
		Scenario:   simScenarioName[run.Scenario],
		Status:     simStatusName(run.Status),
		Steps:      []simStepDTO{},
		StartedAt:  run.StartedAt.UTC(),
		FinishedAt: run.FinishedAt,
	}
	if dto.Scenario == "" {
		dto.Scenario = string(run.Scenario)
	}
	// Detail 为 sim 包写入的 JSON ({steps,summary,error}) — 解码回填响应字段.
	if run.Detail != "" {
		var d runDetailJSON
		if json.Unmarshal([]byte(run.Detail), &d) == nil {
			if d.Steps != nil {
				dto.Steps = d.Steps
			}
			if d.Summary != nil {
				dto.Summary = d.Summary
			}
			if d.Error != "" {
				e := d.Error
				dto.Error = &e
			}
			return dto
		}
		// 非 JSON detail: 作为 error 文本透出.
		e := run.Detail
		dto.Error = &e
	}
	return dto
}

func simStatusName(st ports.SimRunStatus) string {
	switch st {
	case ports.SimRunRunning:
		return "RUNNING"
	case ports.SimRunSucceeded:
		return "COMPLETED"
	case ports.SimRunFailed:
		return "FAILED"
	default:
		return string(st)
	}
}
