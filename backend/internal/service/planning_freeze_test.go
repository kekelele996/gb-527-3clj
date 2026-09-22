package service

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"satellite-contact-window-deconfliction/backend/internal/config"
	"satellite-contact-window-deconfliction/backend/internal/constants"
	"satellite-contact-window-deconfliction/backend/internal/dto"
	"satellite-contact-window-deconfliction/backend/internal/model"
	"satellite-contact-window-deconfliction/backend/internal/repository"
)

type freezeFixture struct {
	service     *ConflictResolutionService
	system      *repository.SystemRepository
	stations    *repository.GroundStationRepository
	assets      *repository.SatelliteAssetRepository
	windows     *repository.ContactWindowRepository
	station     model.GroundStation
	satellites  []model.SatelliteAsset
	windowsList []model.ContactWindow
	scheduler   dto.Actor
	reviewer    dto.Actor
	base        time.Time
}

func newFreezeFixture(t *testing.T, name string) *freezeFixture {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.GroundStation{}, &model.SatelliteAsset{}, &model.ContactWindow{}, &model.ConflictResolution{}, &model.AuditEvent{}); err != nil {
		t.Fatal(err)
	}
	station := model.GroundStation{StationCode: "FRZ-GS", Name: "Freeze Test", AntennaCount: 1, SupportedBandsJSON: `["S"]`, StationStatus: "active", Version: 1}
	if err := db.Create(&station).Error; err != nil {
		t.Fatal(err)
	}
	satellites := []model.SatelliteAsset{
		{SatelliteCode: "FRZ-A", Name: "A", SupportedBandsJSON: `["S"]`, MinimumContactSec: 60, AssetStatus: "active", PriorityWeight: 2, Version: 1},
		{SatelliteCode: "FRZ-B", Name: "B", SupportedBandsJSON: `["S"]`, MinimumContactSec: 60, AssetStatus: "active", PriorityWeight: 1, Version: 1},
	}
	if err := db.Create(&satellites).Error; err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Second)
	windows := []model.ContactWindow{
		{StationID: station.ID, SatelliteID: satellites[0].ID, StartAt: base, EndAt: base.Add(10 * time.Minute), Band: "S", WindowStatus: constants.WindowStatusSubmitted, Priority: 8, SourceVersion: "freeze-source", Version: 1},
		{StationID: station.ID, SatelliteID: satellites[1].ID, StartAt: base.Add(time.Minute), EndAt: base.Add(9 * time.Minute), Band: "S", WindowStatus: constants.WindowStatusSubmitted, Priority: 5, SourceVersion: "freeze-source", Version: 1},
	}
	if err := db.Create(&windows).Error; err != nil {
		t.Fatal(err)
	}
	stationRepository := repository.NewGroundStationRepository(db)
	assetRepository := repository.NewSatelliteAssetRepository(db)
	windowRepository := repository.NewContactWindowRepository(db)
	systemRepository := repository.NewSystemRepository(db)
	service := NewConflictResolutionService(repository.NewConflictResolutionRepository(db), windowRepository, stationRepository, assetRepository, NewAuditService(systemRepository), config.Weights{PriorityLoss: 4, MovementDistance: .02, ContactDuration: .003, ResourceMargin: 2})
	return &freezeFixture{
		service: service, system: systemRepository, stations: stationRepository, assets: assetRepository, windows: windowRepository,
		station: station, satellites: satellites, windowsList: windows, base: base,
		scheduler: dto.Actor{ID: 1, Username: "scheduler", Role: constants.RoleScheduler},
		reviewer:  dto.Actor{ID: 2, Username: "reviewer", Role: constants.RoleReviewer},
	}
}

func (fixture *freezeFixture) detectAndSubmit(t *testing.T) dto.ConflictResolutionResponse {
	t.Helper()
	detected, err := fixture.service.Detect(dto.DetectConflictsRequest{From: fixture.base.Add(-time.Minute).Format(time.RFC3339), To: fixture.base.Add(time.Hour).Format(time.RFC3339)}, fixture.scheduler, "freeze-detect")
	if err != nil {
		t.Fatal(err)
	}
	var target dto.ConflictResolutionResponse
	for _, resolution := range detected.Resolutions {
		if resolution.ConflictType == constants.ConflictTypeStationCapacity {
			target = resolution
			break
		}
	}
	if target.ID == 0 || target.ResolutionStatus != constants.ResolutionStatusProposed {
		t.Fatalf("expected proposed station conflict, got %+v", target)
	}
	if target.Freeze.Status != constants.FreezeStatusIntact || len(target.Freeze.Violations) != 0 {
		t.Fatalf("fresh scan must report an intact freeze, got %+v", target.Freeze)
	}
	submitted, err := fixture.service.Submit(target.ID, dto.ConflictActionRequest{ExpectedVersion: target.Version}, fixture.scheduler, "freeze-submit")
	if err != nil {
		t.Fatal(err)
	}
	return submitted
}

func (fixture *freezeFixture) auditEvents(t *testing.T, action string) []model.AuditEvent {
	t.Helper()
	events, _, err := fixture.system.ListAudit(1, 100, "conflict_resolution", action)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestFreezeSnapshotRecordedAtScan(t *testing.T) {
	fixture := newFreezeFixture(t, "freeze-snapshot")
	target := fixture.detectAndSubmit(t)
	if target.Freeze.FrozenAt.IsZero() {
		t.Fatal("freeze timestamp must be recorded at scan time")
	}
	stored, err := fixture.service.repository.Get(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	var frozen dto.FrozenInputs
	if err := json.Unmarshal([]byte(stored.FrozenInputsJSON), &frozen); err != nil {
		t.Fatal(err)
	}
	if len(frozen.Windows) != 2 || len(frozen.Stations) != 1 || len(frozen.Satellites) != 2 {
		t.Fatalf("expected 2 windows, 1 station and 2 satellites frozen, got %+v", frozen)
	}
	if frozen.Stations[0].AntennaCount != 1 || frozen.Stations[0].StationStatus != "active" {
		t.Fatalf("station snapshot mismatch %+v", frozen.Stations[0])
	}
	if frozen.Satellites[0].MinimumContactSec != 60 || frozen.Windows[0].Priority != 8 {
		t.Fatalf("snapshot values mismatch %+v", frozen)
	}
}

func TestFreezeBlocksAcceptWhenStationCapacityChanges(t *testing.T) {
	fixture := newFreezeFixture(t, "freeze-capacity")
	target := fixture.detectAndSubmit(t)
	beforeEvents := fixture.auditEvents(t, "")
	if _, err := fixture.stations.Update(fixture.station.ID, fixture.station.Version, map[string]any{"antenna_count": 2}); err != nil {
		t.Fatal(err)
	}
	loaded, err := fixture.service.Get(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Freeze.Status != constants.FreezeStatusViolated {
		t.Fatalf("expected violated freeze after capacity change, got %+v", loaded.Freeze)
	}
	var capacity *dto.FreezeViolation
	for index, violation := range loaded.Freeze.Violations {
		if violation.ObjectType == constants.FreezeObjectStation && violation.Field == "capacity" {
			capacity = &loaded.Freeze.Violations[index]
		}
	}
	if capacity == nil || capacity.ObjectLabel != "FRZ-GS" || capacity.FrozenValue != "1" || capacity.CurrentValue != "2" {
		t.Fatalf("expected station capacity 1 -> 2 violation, got %+v", loaded.Freeze.Violations)
	}
	_, err = fixture.service.Review(target.ID, dto.ConflictActionRequest{ExpectedVersion: target.Version, Decision: constants.ResolutionStatusAccepted, ActionKey: target.Suggestions[0].ActionKey, ReviewNote: "must not be stored"}, fixture.reviewer, "freeze-accept")
	var appError *AppError
	if !errors.As(err, &appError) || appError.Code != "frozen_inputs_changed" || appError.Status != 409 {
		t.Fatalf("expected 409 frozen_inputs_changed, got %v", err)
	}
	details, ok := appError.Details.(map[string]any)
	if !ok || len(details["violations"].([]dto.FreezeViolation)) == 0 {
		t.Fatalf("expected violation details on the error, got %+v", appError.Details)
	}
	blocked, err := fixture.service.Get(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if blocked.ResolutionStatus != constants.ResolutionStatusPendingReview || blocked.ResolvedBy != "" || blocked.ReviewNote != "" || blocked.SelectedAction != nil {
		t.Fatalf("blocked accept must keep the conflict pending and untouched, got %+v", blocked)
	}
	afterEvents := fixture.auditEvents(t, "")
	if len(afterEvents) != len(beforeEvents)+1 {
		t.Fatalf("blocked accept must append exactly one audit event, had %d now %d", len(beforeEvents), len(afterEvents))
	}
	blockedEvents := fixture.auditEvents(t, "conflict.review_blocked")
	if len(blockedEvents) != 1 || !strings.Contains(blockedEvents[0].AfterSummary, "capacity") {
		t.Fatalf("expected conflict.review_blocked audit with violations, got %+v", blockedEvents)
	}
	if reviewed := fixture.auditEvents(t, "conflict.reviewed"); len(reviewed) != 0 {
		t.Fatalf("no review decision may be audited for a blocked accept, got %+v", reviewed)
	}
	rejected, err := fixture.service.Review(target.ID, dto.ConflictActionRequest{ExpectedVersion: target.Version, Decision: constants.ResolutionStatusRejected, ReviewNote: "Station changed under review"}, fixture.reviewer, "freeze-reject")
	if err != nil {
		t.Fatal(err)
	}
	if rejected.ResolutionStatus != constants.ResolutionStatusRejected || rejected.ReviewNote != "Station changed under review" {
		t.Fatalf("reject must still be saved, got %+v", rejected)
	}
}

func TestFreezeRecoversWhenSatelliteMinimumContactRestored(t *testing.T) {
	fixture := newFreezeFixture(t, "freeze-min-contact")
	target := fixture.detectAndSubmit(t)
	satellite := fixture.satellites[0]
	if _, err := fixture.assets.Update(satellite.ID, satellite.Version, map[string]any{"minimum_contact_sec": 90}); err != nil {
		t.Fatal(err)
	}
	loaded, err := fixture.service.Get(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	var minimum *dto.FreezeViolation
	for index, violation := range loaded.Freeze.Violations {
		if violation.ObjectType == constants.FreezeObjectSatellite && violation.Field == "minimum_contact" {
			minimum = &loaded.Freeze.Violations[index]
		}
	}
	if minimum == nil || minimum.ObjectLabel != "FRZ-A" || minimum.FrozenValue != "60" || minimum.CurrentValue != "90" {
		t.Fatalf("expected satellite minimum_contact 60 -> 90 violation, got %+v", loaded.Freeze.Violations)
	}
	if _, err := fixture.service.Review(target.ID, dto.ConflictActionRequest{ExpectedVersion: target.Version, Decision: constants.ResolutionStatusAccepted, ActionKey: target.Suggestions[0].ActionKey}, fixture.reviewer, "freeze-blocked"); !errors.Is(err, errFrozenInputsChanged) {
		var appError *AppError
		if !errors.As(err, &appError) || appError.Code != "frozen_inputs_changed" {
			t.Fatalf("expected frozen_inputs_changed, got %v", err)
		}
	}
	if _, err := fixture.assets.Update(satellite.ID, satellite.Version+1, map[string]any{"minimum_contact_sec": 60}); err != nil {
		t.Fatal(err)
	}
	restored, err := fixture.service.Get(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Freeze.Status != constants.FreezeStatusIntact || len(restored.Freeze.Violations) != 0 {
		t.Fatalf("restored inputs must clear the violation, got %+v", restored.Freeze)
	}
	accepted, err := fixture.service.Review(target.ID, dto.ConflictActionRequest{ExpectedVersion: target.Version, Decision: constants.ResolutionStatusAccepted, ActionKey: target.Suggestions[0].ActionKey, ReviewNote: "Inputs stable again"}, fixture.reviewer, "freeze-accept")
	if err != nil {
		t.Fatal(err)
	}
	if accepted.ResolutionStatus != constants.ResolutionStatusAccepted || accepted.SelectedAction == nil {
		t.Fatalf("expected accepted resolution with a recorded choice, got %+v", accepted)
	}
	record, err := fixture.service.Export(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if record["resolution_id"] != target.ID {
		t.Fatalf("unexpected export record %+v", record)
	}
}

func TestConcurrentReviewSucceedsOnlyOnce(t *testing.T) {
	fixture := newFreezeFixture(t, "freeze-concurrent")
	target := fixture.detectAndSubmit(t)
	request := dto.ConflictActionRequest{ExpectedVersion: target.Version, Decision: constants.ResolutionStatusAccepted, ActionKey: target.Suggestions[0].ActionKey}
	accepted, err := fixture.service.Review(target.ID, request, fixture.reviewer, "freeze-first")
	if err != nil {
		t.Fatal(err)
	}
	if accepted.ResolutionStatus != constants.ResolutionStatusAccepted {
		t.Fatalf("first review must win, got %+v", accepted)
	}
	_, err = fixture.service.Review(target.ID, request, fixture.reviewer, "freeze-second")
	var appError *AppError
	if !errors.As(err, &appError) || appError.Code != "version_conflict" {
		t.Fatalf("stale concurrent review must fail with version_conflict, got %v", err)
	}
	_, err = fixture.service.Review(target.ID, dto.ConflictActionRequest{ExpectedVersion: accepted.Version, Decision: constants.ResolutionStatusRejected}, fixture.reviewer, "freeze-third")
	if !errors.As(err, &appError) || appError.Code != "invalid_state" {
		t.Fatalf("a decided conflict cannot be reviewed again, got %v", err)
	}
	final, err := fixture.service.Get(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.ResolutionStatus != constants.ResolutionStatusAccepted || final.ResolvedBy != "reviewer" {
		t.Fatalf("exactly one decision may be recorded, got %+v", final)
	}
}

func TestLegacyResolutionWithoutFreezeSnapshotStaysGuarded(t *testing.T) {
	fixture := newFreezeFixture(t, "freeze-legacy")
	legacy := model.ConflictResolution{
		ConflictKey: "legacy-key", WindowIDsJSON: mustJSON([]uint{fixture.windowsList[0].ID}),
		WindowVersionsJSON: mustJSON(map[string]uint{"1": 1}), ConflictType: constants.ConflictTypeStationCapacity,
		EvidenceJSON:    mustJSON(dto.ConflictEvidence{WindowFacts: []map[string]any{}, Metadata: map[string]interface{}{}}),
		SuggestionsJSON: mustJSON([]dto.ResolutionSuggestion{{ActionKey: "manual", ActionType: "manual", Title: "Manual"}}),
		WeightsJSON:     "{}", SelectedAction: "{}", ResolutionStatus: constants.ResolutionStatusPendingReview, Version: 3,
	}
	if err := fixture.service.repository.Create(&legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.windows.Update(fixture.windowsList[0].ID, fixture.windowsList[0].Version, map[string]any{"priority": 9}); err != nil {
		t.Fatal(err)
	}
	loaded, err := fixture.service.Get(legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Freeze.Status != constants.FreezeStatusViolated || len(loaded.Freeze.Violations) != 1 || loaded.Freeze.Violations[0].Field != "version" {
		t.Fatalf("legacy window version change must surface as a violation, got %+v", loaded.Freeze)
	}
	_, err = fixture.service.Review(legacy.ID, dto.ConflictActionRequest{ExpectedVersion: 3, Decision: constants.ResolutionStatusAccepted, ActionKey: "manual"}, fixture.reviewer, "freeze-legacy-accept")
	var appError *AppError
	if !errors.As(err, &appError) || appError.Code != "frozen_inputs_changed" {
		t.Fatalf("expected frozen_inputs_changed for legacy resolution, got %v", err)
	}
}
