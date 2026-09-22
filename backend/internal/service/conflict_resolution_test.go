package service

import (
	"errors"
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

func TestReviewDetectsChangedWindowAndRejectRemainsAvailable(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:conflict-review?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.GroundStation{}, &model.SatelliteAsset{}, &model.ContactWindow{}, &model.ConflictResolution{}, &model.AuditEvent{}); err != nil {
		t.Fatal(err)
	}
	station := model.GroundStation{StationCode: "TEST-GS", Name: "Test", AntennaCount: 1, SupportedBandsJSON: `["S"]`, StationStatus: "active", Version: 1}
	assets := []model.SatelliteAsset{
		{SatelliteCode: "TEST-A", Name: "A", SupportedBandsJSON: `["S"]`, MinimumContactSec: 60, AssetStatus: "active", PriorityWeight: 2, Version: 1},
		{SatelliteCode: "TEST-B", Name: "B", SupportedBandsJSON: `["S"]`, MinimumContactSec: 60, AssetStatus: "active", PriorityWeight: 1, Version: 1},
	}
	if err := db.Create(&station).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&assets).Error; err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Second)
	windows := []model.ContactWindow{
		{StationID: station.ID, SatelliteID: assets[0].ID, StartAt: base, EndAt: base.Add(10 * time.Minute), Band: "S", WindowStatus: constants.WindowStatusSubmitted, Priority: 8, SourceVersion: "test-source", Version: 1},
		{StationID: station.ID, SatelliteID: assets[1].ID, StartAt: base.Add(time.Minute), EndAt: base.Add(9 * time.Minute), Band: "S", WindowStatus: constants.WindowStatusSubmitted, Priority: 5, SourceVersion: "test-source", Version: 1},
	}
	if err := db.Create(&windows).Error; err != nil {
		t.Fatal(err)
	}
	stationRepository := repository.NewGroundStationRepository(db)
	assetRepository := repository.NewSatelliteAssetRepository(db)
	windowRepository := repository.NewContactWindowRepository(db)
	conflictRepository := repository.NewConflictResolutionRepository(db)
	audit := NewAuditService(repository.NewSystemRepository(db))
	service := NewConflictResolutionService(conflictRepository, windowRepository, stationRepository, assetRepository, audit, config.Weights{PriorityLoss: 4, MovementDistance: .02, ContactDuration: .003, ResourceMargin: 2})
	actor := dto.Actor{ID: 1, Username: "scheduler", Role: constants.RoleScheduler}
	detected, err := service.Detect(dto.DetectConflictsRequest{From: base.Add(-time.Minute).Format(time.RFC3339), To: base.Add(time.Hour).Format(time.RFC3339)}, actor, "test-detect")
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
	target, err = service.Submit(target.ID, dto.ConflictActionRequest{ExpectedVersion: target.Version}, actor, "test-submit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := windowRepository.Update(windows[0].ID, windows[0].Version, map[string]any{"priority": 9}); err != nil {
		t.Fatal(err)
	}
	reviewer := dto.Actor{ID: 2, Username: "reviewer", Role: constants.RoleReviewer}
	_, err = service.Review(target.ID, dto.ConflictActionRequest{ExpectedVersion: target.Version, Decision: constants.ResolutionStatusAccepted, ActionKey: target.Suggestions[0].ActionKey}, reviewer, "test-accept")
	var appError *AppError
	if !errors.As(err, &appError) || appError.Code != "frozen_inputs_changed" {
		t.Fatalf("expected frozen_inputs_changed, got %v", err)
	}
	blocked, err := service.Get(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if blocked.ResolutionStatus != constants.ResolutionStatusPendingReview || blocked.ResolvedBy != "" || blocked.ReviewNote != "" {
		t.Fatalf("blocked accept must leave the review untouched, got %+v", blocked)
	}
	if blocked.Freeze.Status != constants.FreezeStatusViolated || len(blocked.Freeze.Violations) == 0 {
		t.Fatalf("expected violated freeze with changed objects, got %+v", blocked.Freeze)
	}
	violation := blocked.Freeze.Violations[0]
	if violation.ObjectType != constants.FreezeObjectWindow || violation.Field != "priority" || violation.FrozenValue != "8" || violation.CurrentValue != "9" {
		t.Fatalf("unexpected violation %+v", violation)
	}
	rejected, err := service.Review(target.ID, dto.ConflictActionRequest{ExpectedVersion: target.Version, Decision: constants.ResolutionStatusRejected, ReviewNote: "Orbit source changed during review"}, reviewer, "test-reject")
	if err != nil {
		t.Fatal(err)
	}
	if rejected.ResolutionStatus != constants.ResolutionStatusRejected {
		t.Fatalf("got %s", rejected.ResolutionStatus)
	}
}
