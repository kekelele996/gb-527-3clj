package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"satellite-contact-window-deconfliction/backend/internal/config"
	"satellite-contact-window-deconfliction/backend/internal/constants"
	"satellite-contact-window-deconfliction/backend/internal/dto"
	"satellite-contact-window-deconfliction/backend/internal/model"
	"satellite-contact-window-deconfliction/backend/internal/repository"
	"satellite-contact-window-deconfliction/backend/internal/scheduler"
)

type ConflictResolutionService struct {
	repository *repository.ConflictResolutionRepository
	windows    *repository.ContactWindowRepository
	stations   *repository.GroundStationRepository
	assets     *repository.SatelliteAssetRepository
	audit      *AuditService
	weights    scheduler.Weights
}

func NewConflictResolutionService(conflicts *repository.ConflictResolutionRepository, windows *repository.ContactWindowRepository, stations *repository.GroundStationRepository, assets *repository.SatelliteAssetRepository, audit *AuditService, weights config.Weights) *ConflictResolutionService {
	return &ConflictResolutionService{
		repository: conflicts, windows: windows, stations: stations, assets: assets, audit: audit,
		weights: scheduler.Weights{PriorityLoss: weights.PriorityLoss, MovementDistance: weights.MovementDistance, ContactDuration: weights.ContactDuration, ResourceMargin: weights.ResourceMargin},
	}
}

func (service *ConflictResolutionService) List(page, pageSize int, status, conflictType string) ([]dto.ConflictResolutionResponse, dto.PageMeta, error) {
	resolutions, total, err := service.repository.List(page, pageSize, status, conflictType)
	if err != nil {
		return nil, dto.PageMeta{}, Internal("could not list conflict resolutions", err)
	}
	responses := make([]dto.ConflictResolutionResponse, 0, len(resolutions))
	for _, resolution := range resolutions {
		response, parseErr := resolutionResponse(resolution)
		if parseErr != nil {
			return nil, dto.PageMeta{}, parseErr
		}
		responses = append(responses, response)
	}
	return responses, pageMeta(page, pageSize, total), nil
}

func (service *ConflictResolutionService) Get(id uint) (dto.ConflictResolutionResponse, error) {
	resolution, err := service.repository.Get(id)
	if err != nil {
		return dto.ConflictResolutionResponse{}, MapRepositoryError("conflict resolution", err)
	}
	return resolutionResponse(resolution)
}

func (service *ConflictResolutionService) Detect(request dto.DetectConflictsRequest, actor dto.Actor, requestID string) (dto.DetectionResult, error) {
	from, err := time.Parse(time.RFC3339, request.From)
	if err != nil {
		return dto.DetectionResult{}, BadRequest("invalid_time_range", "from must be RFC3339")
	}
	to, err := time.Parse(time.RFC3339, request.To)
	if err != nil {
		return dto.DetectionResult{}, BadRequest("invalid_time_range", "to must be RFC3339")
	}
	if !to.After(from) || to.Sub(from) > 31*24*time.Hour {
		return dto.DetectionResult{}, BadRequest("invalid_time_range", "detection range must be positive and no longer than 31 days")
	}
	windows, err := service.windows.ListRange(from.UTC(), to.UTC())
	if err != nil {
		return dto.DetectionResult{}, Internal("could not load windows for detection", err)
	}
	stations, err := service.stations.ListAll()
	if err != nil {
		return dto.DetectionResult{}, Internal("could not load stations for detection", err)
	}
	assets, err := service.assets.ListAll()
	if err != nil {
		return dto.DetectionResult{}, Internal("could not load satellites for detection", err)
	}
	stationMap := map[uint]model.GroundStation{}
	assetMap := map[uint]model.SatelliteAsset{}
	for _, station := range stations {
		stationMap[station.ID] = station
	}
	for _, asset := range assets {
		assetMap[asset.ID] = asset
	}
	groups := scheduler.Detect(scheduler.DetectionContext{Windows: windows, Stations: stationMap, Satellites: assetMap})
	generator := scheduler.NewCandidateGenerator(service.weights, stations, assets, windows)
	responses := make([]dto.ConflictResolutionResponse, 0, len(groups))
	for _, group := range groups {
		resolution, err := service.persistDetection(group, generator.Generate(group), stationMap, assetMap, actor, requestID)
		if err != nil {
			return dto.DetectionResult{}, err
		}
		responses = append(responses, resolution)
	}
	if err := service.audit.Record(actor, requestID, "conflicts.scanned", "planning_range", from.UTC().Format(time.RFC3339), map[string]any{"from": from.UTC(), "to": to.UTC(), "window_count": len(windows), "weights": service.weights}, nil, map[string]any{"conflict_count": len(groups)}); err != nil {
		return dto.DetectionResult{}, err
	}
	return dto.DetectionResult{RangeFrom: from.UTC(), RangeTo: to.UTC(), WindowCount: len(windows), ConflictCount: len(groups), Resolutions: responses}, nil
}

func (service *ConflictResolutionService) persistDetection(group scheduler.ConflictGroup, suggestions []scheduler.Suggestion, stations map[uint]model.GroundStation, assets map[uint]model.SatelliteAsset, actor dto.Actor, requestID string) (dto.ConflictResolutionResponse, error) {
	if existing, err := service.repository.FindByKey(group.Key); err == nil {
		return resolutionResponse(existing)
	}
	windowIDs := make([]uint, 0, len(group.Windows))
	versions := map[string]uint{}
	facts := make([]map[string]any, 0, len(group.Windows))
	for _, window := range group.Windows {
		windowIDs = append(windowIDs, window.ID)
		versions[strconv.FormatUint(uint64(window.ID), 10)] = window.Version
		facts = append(facts, map[string]any{"id": window.ID, "station_id": window.StationID, "satellite_id": window.SatelliteID, "start_at": window.StartAt, "end_at": window.EndAt, "duration_sec": window.DurationSec(), "band": window.Band, "priority": window.Priority, "locked": window.Locked, "version": window.Version})
	}
	snapshot := freezeSnapshot(group.Windows, stations, assets)
	evidence := dto.ConflictEvidence{Summary: group.Summary, WindowFacts: facts, Capacity: group.Capacity, PeakConcurrency: group.PeakConcurrency, BufferSeconds: group.BufferSeconds, Metadata: group.Metadata}
	resolution := model.ConflictResolution{
		ConflictKey: group.Key, WindowIDsJSON: mustJSON(windowIDs), WindowVersionsJSON: mustJSON(versions), ConflictType: group.ConflictType,
		EvidenceJSON: mustJSON(evidence), SuggestionsJSON: mustJSON(suggestions), WeightsJSON: mustJSON(service.weights), SelectedAction: "{}",
		ResolutionStatus: constants.ResolutionStatusDetected, FrozenInputsJSON: mustJSON(snapshot), FreezeStatus: constants.FreezeStatusFrozen,
		FrozenChangesJSON: "[]", Version: 1,
	}
	err := service.repository.DB().Transaction(func(tx *gorm.DB) error {
		txRepository := service.repository.WithDB(tx)
		if err := txRepository.Create(&resolution); err != nil {
			return err
		}
		if err := service.audit.RecordTx(tx, actor, requestID, "conflict.detected", "conflict_resolution", auditID(resolution.ID), map[string]any{"conflict_type": group.ConflictType, "window_ids": windowIDs, "weights": service.weights}, nil, map[string]any{"status": constants.ResolutionStatusDetected, "version": 1, "freeze_status": constants.FreezeStatusFrozen, "frozen_window_count": len(snapshot.Windows), "frozen_station_count": len(snapshot.Stations), "frozen_satellite_count": len(snapshot.Satellites)}); err != nil {
			return err
		}
		updated, err := txRepository.Transition(tx, resolution.ID, 1, constants.ResolutionStatusDetected, constants.ResolutionStatusProposed, map[string]any{})
		if err != nil {
			return err
		}
		if !updated {
			return errors.New("detected resolution could not enter proposed state")
		}
		return service.audit.RecordTx(tx, actor, requestID, "conflict.proposed", "conflict_resolution", auditID(resolution.ID), map[string]any{"suggestion_count": len(suggestions)}, map[string]any{"status": constants.ResolutionStatusDetected}, map[string]any{"status": constants.ResolutionStatusProposed, "version": 2})
	})
	if err != nil {
		if existing, findErr := service.repository.FindByKey(group.Key); findErr == nil {
			return resolutionResponse(existing)
		}
		return dto.ConflictResolutionResponse{}, Internal("could not persist conflict detection", err)
	}
	created, err := service.repository.Get(resolution.ID)
	if err != nil {
		return dto.ConflictResolutionResponse{}, Internal("could not reload conflict resolution", err)
	}
	return resolutionResponse(created)
}

func (service *ConflictResolutionService) Submit(id uint, request dto.ConflictActionRequest, actor dto.Actor, requestID string) (dto.ConflictResolutionResponse, error) {
	before, err := service.repository.Get(id)
	if err != nil {
		return dto.ConflictResolutionResponse{}, MapRepositoryError("conflict resolution", err)
	}
	if !constants.CanTransitionResolution(before.ResolutionStatus, constants.ResolutionStatusPendingReview) {
		return dto.ConflictResolutionResponse{}, Conflict("invalid_state", "only proposed conflicts can be submitted for review", nil)
	}
	updated, err := service.repository.Transition(service.repository.DB(), id, request.ExpectedVersion, constants.ResolutionStatusProposed, constants.ResolutionStatusPendingReview, map[string]any{})
	if err != nil {
		return dto.ConflictResolutionResponse{}, Internal("could not submit conflict resolution", err)
	}
	if !updated {
		return dto.ConflictResolutionResponse{}, Conflict("version_conflict", "conflict resolution changed; reload before submitting", nil)
	}
	after, _ := service.repository.Get(id)
	if err := service.audit.Record(actor, requestID, "conflict.submitted", "conflict_resolution", auditID(id), map[string]any{"expected_version": request.ExpectedVersion}, resolutionSummary(before), resolutionSummary(after)); err != nil {
		return dto.ConflictResolutionResponse{}, err
	}
	return resolutionResponse(after)
}

func (service *ConflictResolutionService) Review(id uint, request dto.ConflictActionRequest, actor dto.Actor, requestID string) (dto.ConflictResolutionResponse, error) {
	if request.Decision != constants.ResolutionStatusAccepted && request.Decision != constants.ResolutionStatusRejected {
		return dto.ConflictResolutionResponse{}, BadRequest("invalid_decision", "decision must be accepted or rejected")
	}
	if request.Decision == constants.ResolutionStatusAccepted && request.ActionKey == "" {
		return dto.ConflictResolutionResponse{}, BadRequest("action_required", "an accepted resolution must select an action")
	}
	var blocked *AppError
	err := service.repository.DB().Transaction(func(tx *gorm.DB) error {
		resolution, err := service.repository.GetForUpdate(tx, id)
		if err != nil {
			return MapRepositoryError("conflict resolution", err)
		}
		if resolution.Version != request.ExpectedVersion {
			return Conflict("version_conflict", "conflict resolution changed; reload before reviewing", nil)
		}
		if !constants.CanTransitionResolution(resolution.ResolutionStatus, request.Decision) {
			return Conflict("invalid_state", "only pending review conflicts can be accepted or rejected", nil)
		}
		values := map[string]any{"resolved_by": actor.Username, "review_note": request.ReviewNote, "resolved_at": time.Now().UTC()}
		if request.Decision == constants.ResolutionStatusAccepted {
			selected, err := selectSuggestion(resolution.SuggestionsJSON, request.ActionKey)
			if err != nil {
				return err
			}
			changes, err := service.evaluateFrozenInputs(tx, resolution)
			if err != nil {
				return err
			}
			if len(changes) > 0 {
				if err := service.repository.RecordFreezeEvaluation(tx, id, constants.FreezeStatusInvalidated, constants.FreezeBlockReasonInputsChanged, mustJSON(changes)); err != nil {
					return Internal("could not record freeze evaluation", err)
				}
				parameters := map[string]any{"decision": request.Decision, "action_key": request.ActionKey, "review_note_length": len(request.ReviewNote), "change_count": len(changes)}
				after := map[string]any{"freeze_status": constants.FreezeStatusInvalidated, "blocked_reason": constants.FreezeBlockReasonInputsChanged, "changed_objects": changes}
				if err := service.audit.RecordTx(tx, actor, requestID, "conflict.accept_blocked", "conflict_resolution", auditID(id), parameters, resolutionSummary(resolution), after); err != nil {
					return err
				}
				blocked = frozenInputBlockError(changes)
				return nil
			}
			values["selected_action"] = mustJSON(selected)
			values["freeze_status"] = constants.FreezeStatusFrozen
			values["frozen_changes_json"] = "[]"
			values["freeze_blocked_reason"] = ""
		}
		updated, err := service.repository.Transition(tx, id, request.ExpectedVersion, constants.ResolutionStatusPendingReview, request.Decision, values)
		if err != nil {
			return err
		}
		if !updated {
			return Conflict("version_conflict", "resolution changed during review", nil)
		}
		parameters := map[string]any{"decision": request.Decision, "action_key": request.ActionKey, "review_note_length": len(request.ReviewNote), "expected_version": request.ExpectedVersion}
		return service.audit.RecordTx(tx, actor, requestID, "conflict.reviewed", "conflict_resolution", auditID(id), parameters, resolutionSummary(resolution), map[string]any{"status": request.Decision, "selected_action_key": request.ActionKey, "resolved_by": actor.Username, "version": request.ExpectedVersion + 1})
	})
	if err != nil {
		return dto.ConflictResolutionResponse{}, err
	}
	if blocked != nil {
		return dto.ConflictResolutionResponse{}, blocked
	}
	return service.Get(id)
}

func (service *ConflictResolutionService) Export(id uint) (map[string]any, error) {
	resolution, err := service.Get(id)
	if err != nil {
		return nil, err
	}
	if resolution.ResolutionStatus != constants.ResolutionStatusAccepted {
		return nil, Conflict("invalid_state", "only accepted resolutions can be exported", nil)
	}
	return map[string]any{
		"record_type": "offline_contact_planning_decision", "resolution_id": resolution.ID, "conflict_key": resolution.ConflictKey,
		"window_ids": resolution.WindowIDs, "selected_action": resolution.SelectedAction, "resolved_by": resolution.ResolvedBy,
		"resolved_at": resolution.ResolvedAt, "freeze_status": resolution.FreezeStatus, "frozen_inputs_validated": resolution.FreezeStatus == constants.FreezeStatusFrozen,
		"disclaimer": "Planning record only; no antenna or spacecraft control command is emitted.",
	}, nil
}

func freezeSnapshot(windows []model.ContactWindow, stations map[uint]model.GroundStation, assets map[uint]model.SatelliteAsset) dto.FrozenInputsSnapshot {
	snapshot := dto.FrozenInputsSnapshot{FrozenAt: time.Now().UTC(), Windows: make([]dto.FrozenWindowInput, 0, len(windows)), Stations: []dto.FrozenStationInput{}, Satellites: []dto.FrozenSatelliteInput{}}
	stationSeen := map[uint]bool{}
	satelliteSeen := map[uint]bool{}
	for _, window := range windows {
		snapshot.Windows = append(snapshot.Windows, dto.FrozenWindowInput{ID: window.ID, Version: window.Version, WindowStatus: window.WindowStatus, Band: window.Band, Priority: window.Priority})
		if !stationSeen[window.StationID] {
			stationSeen[window.StationID] = true
			if station, ok := stations[window.StationID]; ok {
				snapshot.Stations = append(snapshot.Stations, dto.FrozenStationInput{ID: station.ID, Version: station.Version, StationCode: station.StationCode, AntennaCount: station.AntennaCount, SupportedBands: decodeStrings(station.SupportedBandsJSON), StationStatus: station.StationStatus})
			}
		}
		if !satelliteSeen[window.SatelliteID] {
			satelliteSeen[window.SatelliteID] = true
			if asset, ok := assets[window.SatelliteID]; ok {
				snapshot.Satellites = append(snapshot.Satellites, dto.FrozenSatelliteInput{ID: asset.ID, Version: asset.Version, SatelliteCode: asset.SatelliteCode, SupportedBands: decodeStrings(asset.SupportedBandsJSON), PriorityWeight: asset.PriorityWeight, MinimumContactSec: asset.MinimumContactSec, AssetStatus: asset.AssetStatus})
			}
		}
	}
	sort.Slice(snapshot.Stations, func(i, j int) bool { return snapshot.Stations[i].ID < snapshot.Stations[j].ID })
	sort.Slice(snapshot.Satellites, func(i, j int) bool { return snapshot.Satellites[i].ID < snapshot.Satellites[j].ID })
	return snapshot
}

func (service *ConflictResolutionService) evaluateFrozenInputs(tx *gorm.DB, resolution model.ConflictResolution) ([]dto.FrozenInputChange, error) {
	if strings.TrimSpace(resolution.FrozenInputsJSON) == "" {
		return service.evaluateLegacyWindowVersions(tx, resolution.WindowVersionsJSON)
	}
	snapshot := dto.FrozenInputsSnapshot{}
	if err := json.Unmarshal([]byte(resolution.FrozenInputsJSON), &snapshot); err != nil {
		return nil, Internal("stored frozen planning inputs are invalid", err)
	}
	changes := make([]dto.FrozenInputChange, 0)
	for _, frozen := range snapshot.Windows {
		window, err := service.windows.FindForUpdate(tx, frozen.ID)
		if err != nil {
			return nil, MapRepositoryError("contact window", err)
		}
		label := fmt.Sprintf("#%d", window.ID)
		if window.Version != frozen.Version {
			changes = append(changes, dto.FrozenInputChange{ObjectType: constants.FrozenObjectContactWindow, ObjectID: window.ID, Label: label, Field: "version", Before: frozen.Version, After: window.Version})
		}
		if window.WindowStatus != frozen.WindowStatus {
			changes = append(changes, dto.FrozenInputChange{ObjectType: constants.FrozenObjectContactWindow, ObjectID: window.ID, Label: label, Field: "window_status", Before: frozen.WindowStatus, After: window.WindowStatus})
		}
		if window.Band != frozen.Band {
			changes = append(changes, dto.FrozenInputChange{ObjectType: constants.FrozenObjectContactWindow, ObjectID: window.ID, Label: label, Field: "band", Before: frozen.Band, After: window.Band})
		}
		if window.Priority != frozen.Priority {
			changes = append(changes, dto.FrozenInputChange{ObjectType: constants.FrozenObjectContactWindow, ObjectID: window.ID, Label: label, Field: "priority", Before: frozen.Priority, After: window.Priority})
		}
	}
	for _, frozen := range snapshot.Stations {
		station, err := service.stations.FindForUpdate(tx, frozen.ID)
		if err != nil {
			return nil, MapRepositoryError("ground station", err)
		}
		if station.AntennaCount != frozen.AntennaCount {
			changes = append(changes, dto.FrozenInputChange{ObjectType: constants.FrozenObjectGroundStation, ObjectID: station.ID, Label: station.StationCode, Field: "antenna_count", Before: frozen.AntennaCount, After: station.AntennaCount})
		}
		if current := decodeStrings(station.SupportedBandsJSON); !equalStringSets(current, frozen.SupportedBands) {
			changes = append(changes, dto.FrozenInputChange{ObjectType: constants.FrozenObjectGroundStation, ObjectID: station.ID, Label: station.StationCode, Field: "supported_bands", Before: frozen.SupportedBands, After: current})
		}
		if station.StationStatus != frozen.StationStatus {
			changes = append(changes, dto.FrozenInputChange{ObjectType: constants.FrozenObjectGroundStation, ObjectID: station.ID, Label: station.StationCode, Field: "station_status", Before: frozen.StationStatus, After: station.StationStatus})
		}
	}
	for _, frozen := range snapshot.Satellites {
		asset, err := service.assets.FindForUpdate(tx, frozen.ID)
		if err != nil {
			return nil, MapRepositoryError("satellite asset", err)
		}
		if current := decodeStrings(asset.SupportedBandsJSON); !equalStringSets(current, frozen.SupportedBands) {
			changes = append(changes, dto.FrozenInputChange{ObjectType: constants.FrozenObjectSatelliteAsset, ObjectID: asset.ID, Label: asset.SatelliteCode, Field: "supported_bands", Before: frozen.SupportedBands, After: current})
		}
		if asset.PriorityWeight != frozen.PriorityWeight {
			changes = append(changes, dto.FrozenInputChange{ObjectType: constants.FrozenObjectSatelliteAsset, ObjectID: asset.ID, Label: asset.SatelliteCode, Field: "priority_weight", Before: frozen.PriorityWeight, After: asset.PriorityWeight})
		}
		if asset.MinimumContactSec != frozen.MinimumContactSec {
			changes = append(changes, dto.FrozenInputChange{ObjectType: constants.FrozenObjectSatelliteAsset, ObjectID: asset.ID, Label: asset.SatelliteCode, Field: "minimum_contact_sec", Before: frozen.MinimumContactSec, After: asset.MinimumContactSec})
		}
		if asset.AssetStatus != frozen.AssetStatus {
			changes = append(changes, dto.FrozenInputChange{ObjectType: constants.FrozenObjectSatelliteAsset, ObjectID: asset.ID, Label: asset.SatelliteCode, Field: "asset_status", Before: frozen.AssetStatus, After: asset.AssetStatus})
		}
	}
	return changes, nil
}

func (service *ConflictResolutionService) evaluateLegacyWindowVersions(tx *gorm.DB, encoded string) ([]dto.FrozenInputChange, error) {
	versions := map[string]uint{}
	if err := json.Unmarshal([]byte(encoded), &versions); err != nil {
		return nil, Internal("stored window version snapshot is invalid", err)
	}
	keys := make([]string, 0, len(versions))
	for id := range versions {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	changes := make([]dto.FrozenInputChange, 0)
	for _, encodedID := range keys {
		parsed, err := strconv.ParseUint(encodedID, 10, 64)
		if err != nil {
			return nil, Internal("stored window ID is invalid", err)
		}
		window, err := service.windows.FindForUpdate(tx, uint(parsed))
		if err != nil {
			return nil, MapRepositoryError("contact window", err)
		}
		if window.Version != versions[encodedID] {
			changes = append(changes, dto.FrozenInputChange{ObjectType: constants.FrozenObjectContactWindow, ObjectID: window.ID, Label: fmt.Sprintf("#%d", window.ID), Field: "version", Before: versions[encodedID], After: window.Version})
		}
	}
	return changes, nil
}

func frozenInputBlockError(changes []dto.FrozenInputChange) *AppError {
	code := "frozen_input_changed"
	message := "frozen planning inputs changed after the conflict scan; acceptance is blocked and the conflict stays pending review"
	for _, change := range changes {
		if change.ObjectType == constants.FrozenObjectContactWindow && change.Field == "version" {
			code = "version_conflict"
			message = "frozen planning inputs changed after the conflict scan; window versions no longer match and acceptance is blocked"
			break
		}
	}
	return Conflict(code, message, nil).WithDetails(map[string]any{"blocked_reason": constants.FreezeBlockReasonInputsChanged, "changed_objects": changes})
}

func equalStringSets(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	counts := map[string]int{}
	for _, value := range left {
		counts[value]++
	}
	for _, value := range right {
		counts[value]--
		if counts[value] < 0 {
			return false
		}
	}
	return true
}

func resolutionResponse(resolution model.ConflictResolution) (dto.ConflictResolutionResponse, error) {
	response := dto.ConflictResolutionResponse{
		ID: resolution.ID, ConflictKey: resolution.ConflictKey, ConflictType: resolution.ConflictType, ResolutionStatus: resolution.ResolutionStatus,
		ResolvedBy: resolution.ResolvedBy, ReviewNote: resolution.ReviewNote, FreezeStatus: resolution.FreezeStatus,
		FreezeBlockedReason: resolution.FreezeBlockedReason, FrozenChanges: []dto.FrozenInputChange{},
		Version: resolution.Version, ResolvedAt: resolution.ResolvedAt, CreatedAt: resolution.CreatedAt, UpdatedAt: resolution.UpdatedAt,
	}
	if response.FreezeStatus == "" {
		response.FreezeStatus = constants.FreezeStatusFrozen
	}
	if err := json.Unmarshal([]byte(resolution.WindowIDsJSON), &response.WindowIDs); err != nil {
		return response, Internal("stored window IDs are invalid", err)
	}
	if err := json.Unmarshal([]byte(resolution.EvidenceJSON), &response.Evidence); err != nil {
		return response, Internal("stored conflict evidence is invalid", err)
	}
	if err := json.Unmarshal([]byte(resolution.SuggestionsJSON), &response.Suggestions); err != nil {
		return response, Internal("stored suggestions are invalid", err)
	}
	if strings.TrimSpace(resolution.FrozenInputsJSON) != "" {
		snapshot := dto.FrozenInputsSnapshot{}
		if err := json.Unmarshal([]byte(resolution.FrozenInputsJSON), &snapshot); err != nil {
			return response, Internal("stored frozen planning inputs are invalid", err)
		}
		response.FrozenInputs = &snapshot
	}
	if strings.TrimSpace(resolution.FrozenChangesJSON) != "" {
		if err := json.Unmarshal([]byte(resolution.FrozenChangesJSON), &response.FrozenChanges); err != nil {
			return response, Internal("stored frozen input changes are invalid", err)
		}
	}
	if resolution.SelectedAction != "" && resolution.SelectedAction != "{}" {
		selected := dto.ResolutionSuggestion{}
		if err := json.Unmarshal([]byte(resolution.SelectedAction), &selected); err != nil {
			return response, Internal("stored selected action is invalid", err)
		}
		response.SelectedAction = &selected
	}
	return response, nil
}

func selectSuggestion(encoded, key string) (dto.ResolutionSuggestion, error) {
	var suggestions []dto.ResolutionSuggestion
	if err := json.Unmarshal([]byte(encoded), &suggestions); err != nil {
		return dto.ResolutionSuggestion{}, Internal("stored suggestions are invalid", err)
	}
	for _, suggestion := range suggestions {
		if suggestion.ActionKey == key {
			return suggestion, nil
		}
	}
	return dto.ResolutionSuggestion{}, BadRequest("unknown_action", "selected action is not part of this resolution")
}

func mustJSON(value any) string { encoded, _ := json.Marshal(value); return string(encoded) }
func resolutionSummary(resolution model.ConflictResolution) map[string]any {
	return map[string]any{"conflict_type": resolution.ConflictType, "status": resolution.ResolutionStatus, "version": resolution.Version, "resolved_by": resolution.ResolvedBy}
}

var _ = errors.Is
