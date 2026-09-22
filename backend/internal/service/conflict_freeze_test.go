package service

import (
	"errors"
	"fmt"
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
	db        *gorm.DB
	service   *ConflictResolutionService
	stations  *repository.GroundStationRepository
	assets    *repository.SatelliteAssetRepository
	windows   *repository.ContactWindowRepository
	station   model.GroundStation
	satellite model.SatelliteAsset
	windowsIn []model.ContactWindow
	target    dto.ConflictResolutionResponse
	scheduler dto.Actor
	reviewer  dto.Actor
}

func newFreezeFixture(t *testing.T, name string) *freezeFixture {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", name)), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.GroundStation{}, &model.SatelliteAsset{}, &model.ContactWindow{}, &model.ConflictResolution{}, &model.AuditEvent{}); err != nil {
		t.Fatal(err)
	}
	station := model.GroundStation{StationCode: "FRZ-GS", Name: "Freeze Ground", AntennaCount: 1, SupportedBandsJSON: `["S","X"]`, StationStatus: "active", Version: 1}
	satellites := []model.SatelliteAsset{
		{SatelliteCode: "FRZ-A", Name: "Freeze A", SupportedBandsJSON: `["S"]`, MinimumContactSec: 60, AssetStatus: "active", PriorityWeight: 2, Version: 1},
		{SatelliteCode: "FRZ-B", Name: "Freeze B", SupportedBandsJSON: `["S"]`, MinimumContactSec: 60, AssetStatus: "active", PriorityWeight: 1, Version: 1},
	}
	if err := db.Create(&station).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&satellites).Error; err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Second)
	windows := []model.ContactWindow{
		{StationID: station.ID, SatelliteID: satellites[0].ID, StartAt: base, EndAt: base.Add(10 * time.Minute), Band: "S", WindowStatus: constants.WindowStatusSubmitted, Priority: 8, SourceVersion: "freeze-test", Version: 1},
		{StationID: station.ID, SatelliteID: satellites[1].ID, StartAt: base.Add(time.Minute), EndAt: base.Add(9 * time.Minute), Band: "S", WindowStatus: constants.WindowStatusSubmitted, Priority: 5, SourceVersion: "freeze-test", Version: 1},
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
	fixture := &freezeFixture{
		db: db, service: service, stations: stationRepository, assets: assetRepository, windows: windowRepository,
		station: station, satellite: satellites[0], windowsIn: windows,
		scheduler: dto.Actor{ID: 1, Username: "scheduler", Role: constants.RoleScheduler},
		reviewer:  dto.Actor{ID: 2, Username: "reviewer", Role: constants.RoleReviewer},
	}
	detected, err := service.Detect(dto.DetectConflictsRequest{From: base.Add(-time.Minute).Format(time.RFC3339), To: base.Add(time.Hour).Format(time.RFC3339)}, fixture.scheduler, "freeze-detect")
	if err != nil {
		t.Fatal(err)
	}
	for _, resolution := range detected.Resolutions {
		if resolution.ConflictType == constants.ConflictTypeStationCapacity {
			fixture.target = resolution
			break
		}
	}
	if fixture.target.ID == 0 {
		t.Fatal("expected a station capacity conflict")
	}
	fixture.target, err = service.Submit(fixture.target.ID, dto.ConflictActionRequest{ExpectedVersion: fixture.target.Version}, fixture.scheduler, "freeze-submit")
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (fixture *freezeFixture) acceptAttempt(t *testing.T) error {
	t.Helper()
	_, err := fixture.service.Review(fixture.target.ID, dto.ConflictActionRequest{ExpectedVersion: fixture.target.Version, Decision: constants.ResolutionStatusAccepted, ActionKey: fixture.target.Suggestions[0].ActionKey, ReviewNote: "attempted note"}, fixture.reviewer, "freeze-accept")
	return err
}

func (fixture *freezeFixture) reload(t *testing.T) dto.ConflictResolutionResponse {
	t.Helper()
	resolution, err := fixture.service.Get(fixture.target.ID)
	if err != nil {
		t.Fatal(err)
	}
	return resolution
}

func TestFreezeSnapshotCapturedAtDetection(t *testing.T) {
	fixture := newFreezeFixture(t, "freeze-snapshot")
	resolution := fixture.reload(t)
	if resolution.FreezeStatus != constants.FreezeStatusFrozen {
		t.Fatalf("expected frozen status, got %q", resolution.FreezeStatus)
	}
	if resolution.FrozenInputs == nil {
		t.Fatal("expected a frozen inputs snapshot")
	}
	if len(resolution.FrozenInputs.Windows) != 2 || len(resolution.FrozenInputs.Stations) != 1 || len(resolution.FrozenInputs.Satellites) != 2 {
		t.Fatalf("unexpected snapshot shape: %+v", resolution.FrozenInputs)
	}
	station := resolution.FrozenInputs.Stations[0]
	if station.AntennaCount != 1 || station.StationStatus != "active" || station.Version != 1 {
		t.Fatalf("unexpected frozen station: %+v", station)
	}
	if len(resolution.FrozenChanges) != 0 {
		t.Fatalf("expected no frozen changes, got %+v", resolution.FrozenChanges)
	}
}

func TestAcceptBlockedWhenStationCapacityChanges(t *testing.T) {
	fixture := newFreezeFixture(t, "freeze-capacity")
	if _, err := fixture.stations.Update(fixture.station.ID, fixture.station.Version, map[string]any{"antenna_count": 2}); err != nil {
		t.Fatal(err)
	}
	var auditBefore int64
	if err := fixture.db.Model(&model.AuditEvent{}).Count(&auditBefore).Error; err != nil {
		t.Fatal(err)
	}
	err := fixture.acceptAttempt(t)
	var appError *AppError
	if !errors.As(err, &appError) || appError.Code != "frozen_input_changed" {
		t.Fatalf("expected frozen_input_changed, got %v", err)
	}
	details, ok := appError.Details.(map[string]any)
	if !ok {
		t.Fatalf("expected error details, got %+v", appError.Details)
	}
	changed, ok := details["changed_objects"].([]dto.FrozenInputChange)
	if !ok || len(changed) != 1 {
		t.Fatalf("expected one changed object, got %+v", details["changed_objects"])
	}
	if changed[0].ObjectType != constants.FrozenObjectGroundStation || changed[0].Field != "antenna_count" || changed[0].Before != 1 || changed[0].After != 2 {
		t.Fatalf("unexpected change entry: %+v", changed[0])
	}
	resolution := fixture.reload(t)
	if resolution.ResolutionStatus != constants.ResolutionStatusPendingReview {
		t.Fatalf("conflict must stay pending_review, got %s", resolution.ResolutionStatus)
	}
	if resolution.FreezeStatus != constants.FreezeStatusInvalidated || resolution.FreezeBlockedReason != constants.FreezeBlockReasonInputsChanged {
		t.Fatalf("expected invalidated freeze, got %s / %s", resolution.FreezeStatus, resolution.FreezeBlockedReason)
	}
	if len(resolution.FrozenChanges) != 1 || resolution.FrozenChanges[0].Field != "antenna_count" {
		t.Fatalf("expected persisted changes, got %+v", resolution.FrozenChanges)
	}
	if resolution.ReviewNote != "" || resolution.ResolvedBy != "" || resolution.SelectedAction != nil {
		t.Fatalf("blocked accept must not record opinions: %+v", resolution)
	}
	var auditAfter int64
	if err := fixture.db.Model(&model.AuditEvent{}).Count(&auditAfter).Error; err != nil {
		t.Fatal(err)
	}
	if auditAfter != auditBefore+1 {
		t.Fatalf("expected exactly one new audit event, got %d -> %d", auditBefore, auditAfter)
	}
	var blocked model.AuditEvent
	if err := fixture.db.Where("action = ?", "conflict.accept_blocked").First(&blocked).Error; err != nil {
		t.Fatalf("expected conflict.accept_blocked audit event: %v", err)
	}
	rejected, err := fixture.service.Review(fixture.target.ID, dto.ConflictActionRequest{ExpectedVersion: fixture.target.Version, Decision: constants.ResolutionStatusRejected, ReviewNote: "inputs drifted"}, fixture.reviewer, "freeze-reject")
	if err != nil {
		t.Fatalf("rejection must still be saved: %v", err)
	}
	if rejected.ResolutionStatus != constants.ResolutionStatusRejected || rejected.ReviewNote != "inputs drifted" {
		t.Fatalf("unexpected rejected resolution: %+v", rejected)
	}
}

func TestAcceptBlockedForEachFrozenAttribute(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(fixture *freezeFixture) error
		field  string
	}{
		{"station bands", func(f *freezeFixture) error {
			_, err := f.stations.Update(f.station.ID, f.station.Version, map[string]any{"supported_bands_json": `["X"]`})
			return err
		}, "supported_bands"},
		{"station status", func(f *freezeFixture) error {
			_, err := f.stations.Update(f.station.ID, f.station.Version, map[string]any{"station_status": "maintenance"})
			return err
		}, "station_status"},
		{"satellite minimum contact", func(f *freezeFixture) error {
			_, err := f.assets.Update(f.satellite.ID, f.satellite.Version, map[string]any{"minimum_contact_sec": 600})
			return err
		}, "minimum_contact_sec"},
		{"satellite priority weight", func(f *freezeFixture) error {
			_, err := f.assets.Update(f.satellite.ID, f.satellite.Version, map[string]any{"priority_weight": 9.5})
			return err
		}, "priority_weight"},
		{"satellite status", func(f *freezeFixture) error {
			_, err := f.assets.Update(f.satellite.ID, f.satellite.Version, map[string]any{"asset_status": "standby"})
			return err
		}, "asset_status"},
	}
	for index, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newFreezeFixture(t, fmt.Sprintf("freeze-attr-%d", index))
			if err := testCase.mutate(fixture); err != nil {
				t.Fatal(err)
			}
			err := fixture.acceptAttempt(t)
			var appError *AppError
			if !errors.As(err, &appError) || appError.Code != "frozen_input_changed" {
				t.Fatalf("expected frozen_input_changed, got %v", err)
			}
			resolution := fixture.reload(t)
			if resolution.ResolutionStatus != constants.ResolutionStatusPendingReview || resolution.FreezeStatus != constants.FreezeStatusInvalidated {
				t.Fatalf("expected pending_review + invalidated, got %s + %s", resolution.ResolutionStatus, resolution.FreezeStatus)
			}
			found := false
			for _, change := range resolution.FrozenChanges {
				if change.Field == testCase.field {
					found = true
				}
			}
			if !found {
				t.Fatalf("expected a %s change, got %+v", testCase.field, resolution.FrozenChanges)
			}
		})
	}
}

func TestAcceptSucceedsWhenFrozenInputsValidAndOnlyOnce(t *testing.T) {
	fixture := newFreezeFixture(t, "freeze-valid")
	accepted, err := fixture.service.Review(fixture.target.ID, dto.ConflictActionRequest{ExpectedVersion: fixture.target.Version, Decision: constants.ResolutionStatusAccepted, ActionKey: fixture.target.Suggestions[0].ActionKey, ReviewNote: "looks good"}, fixture.reviewer, "freeze-accept-ok")
	if err != nil {
		t.Fatal(err)
	}
	if accepted.ResolutionStatus != constants.ResolutionStatusAccepted || accepted.SelectedAction == nil {
		t.Fatalf("expected accepted resolution with a selected action, got %+v", accepted)
	}
	if accepted.FreezeStatus != constants.FreezeStatusFrozen || len(accepted.FrozenChanges) != 0 {
		t.Fatalf("expected clean frozen state, got %s %+v", accepted.FreezeStatus, accepted.FrozenChanges)
	}
	record, err := fixture.service.Export(fixture.target.ID)
	if err != nil {
		t.Fatalf("accepted resolution must be exportable: %v", err)
	}
	if record["freeze_status"] != constants.FreezeStatusFrozen || record["frozen_inputs_validated"] != true {
		t.Fatalf("export must carry freeze proof, got %+v", record)
	}
	_, err = fixture.service.Review(fixture.target.ID, dto.ConflictActionRequest{ExpectedVersion: fixture.target.Version, Decision: constants.ResolutionStatusAccepted, ActionKey: fixture.target.Suggestions[0].ActionKey}, fixture.reviewer, "freeze-accept-twice")
	var appError *AppError
	if !errors.As(err, &appError) || appError.Code != "version_conflict" {
		t.Fatalf("expected version_conflict for a concurrent review, got %v", err)
	}
	reloaded := fixture.reload(t)
	if reloaded.ResolutionStatus != constants.ResolutionStatusAccepted || reloaded.ResolvedBy != "reviewer" {
		t.Fatalf("resolution must stay accepted exactly once, got %+v", reloaded)
	}
}

func TestWindowVersionDriftStillBlocksWithDetails(t *testing.T) {
	fixture := newFreezeFixture(t, "freeze-window-drift")
	if _, err := fixture.windows.Update(fixture.windowsIn[0].ID, fixture.windowsIn[0].Version, map[string]any{"priority": 9}); err != nil {
		t.Fatal(err)
	}
	err := fixture.acceptAttempt(t)
	var appError *AppError
	if !errors.As(err, &appError) || appError.Code != "version_conflict" {
		t.Fatalf("expected version_conflict, got %v", err)
	}
	resolution := fixture.reload(t)
	if resolution.FreezeStatus != constants.FreezeStatusInvalidated || len(resolution.FrozenChanges) == 0 {
		t.Fatalf("expected invalidated freeze with changes, got %s %+v", resolution.FreezeStatus, resolution.FrozenChanges)
	}
	foundWindow := false
	for _, change := range resolution.FrozenChanges {
		if change.ObjectType == constants.FrozenObjectContactWindow && change.Field == "priority" {
			foundWindow = true
		}
	}
	if !foundWindow {
		t.Fatalf("expected a window priority change entry, got %+v", resolution.FrozenChanges)
	}
}
