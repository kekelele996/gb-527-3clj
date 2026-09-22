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

	"satellite-contact-window-deconfliction/backend/internal/constants"
	"satellite-contact-window-deconfliction/backend/internal/dto"
	"satellite-contact-window-deconfliction/backend/internal/model"
)

// errFrozenInputsChanged aborts the review transaction when a frozen planning
// input no longer matches the values captured during conflict detection.
var errFrozenInputsChanged = errors.New("frozen planning inputs changed")

// buildFrozenInputs snapshots the review-relevant fields of every window,
// station and satellite participating in a conflict group. The snapshot is
// stored on the resolution at scan time and is never rewritten afterwards.
func buildFrozenInputs(windows []model.ContactWindow, stations map[uint]model.GroundStation, satellites map[uint]model.SatelliteAsset, frozenAt time.Time) dto.FrozenInputs {
	frozen := dto.FrozenInputs{FrozenAt: frozenAt, Windows: []dto.FrozenWindowInput{}, Stations: []dto.FrozenStationInput{}, Satellites: []dto.FrozenSatelliteInput{}}
	stationIDs := map[uint]bool{}
	satelliteIDs := map[uint]bool{}
	for _, window := range windows {
		frozen.Windows = append(frozen.Windows, dto.FrozenWindowInput{ID: window.ID, Version: window.Version, Band: window.Band, WindowStatus: window.WindowStatus, Priority: window.Priority})
		stationIDs[window.StationID] = true
		satelliteIDs[window.SatelliteID] = true
	}
	for id := range stationIDs {
		station, ok := stations[id]
		if !ok {
			continue
		}
		frozen.Stations = append(frozen.Stations, dto.FrozenStationInput{ID: station.ID, Code: station.StationCode, Version: station.Version, AntennaCount: station.AntennaCount, SupportedBands: decodeBands(station.SupportedBandsJSON), StationStatus: station.StationStatus})
	}
	for id := range satelliteIDs {
		asset, ok := satellites[id]
		if !ok {
			continue
		}
		frozen.Satellites = append(frozen.Satellites, dto.FrozenSatelliteInput{ID: asset.ID, Code: asset.SatelliteCode, Version: asset.Version, SupportedBands: decodeBands(asset.SupportedBandsJSON), AssetStatus: asset.AssetStatus, PriorityWeight: asset.PriorityWeight, MinimumContactSec: asset.MinimumContactSec})
	}
	sort.Slice(frozen.Windows, func(i, j int) bool { return frozen.Windows[i].ID < frozen.Windows[j].ID })
	sort.Slice(frozen.Stations, func(i, j int) bool { return frozen.Stations[i].ID < frozen.Stations[j].ID })
	sort.Slice(frozen.Satellites, func(i, j int) bool { return frozen.Satellites[i].ID < frozen.Satellites[j].ID })
	return frozen
}

// parseFrozenInputs decodes the stored snapshot. Resolutions persisted before
// the freeze existed fall back to the legacy window-version snapshot so their
// accept path stays guarded.
func parseFrozenInputs(resolution model.ConflictResolution) (dto.FrozenInputs, error) {
	if resolution.FrozenInputsJSON == "" || resolution.FrozenInputsJSON == "{}" {
		return legacyFrozenInputs(resolution)
	}
	var frozen dto.FrozenInputs
	if err := json.Unmarshal([]byte(resolution.FrozenInputsJSON), &frozen); err != nil {
		return dto.FrozenInputs{}, Internal("stored frozen planning inputs are invalid", err)
	}
	if frozen.Windows == nil {
		frozen.Windows = []dto.FrozenWindowInput{}
	}
	if frozen.Stations == nil {
		frozen.Stations = []dto.FrozenStationInput{}
	}
	if frozen.Satellites == nil {
		frozen.Satellites = []dto.FrozenSatelliteInput{}
	}
	return frozen, nil
}

func legacyFrozenInputs(resolution model.ConflictResolution) (dto.FrozenInputs, error) {
	versions := map[string]uint{}
	if err := json.Unmarshal([]byte(resolution.WindowVersionsJSON), &versions); err != nil {
		return dto.FrozenInputs{}, Internal("stored window version snapshot is invalid", err)
	}
	frozen := dto.FrozenInputs{FrozenAt: resolution.CreatedAt, Windows: []dto.FrozenWindowInput{}, Stations: []dto.FrozenStationInput{}, Satellites: []dto.FrozenSatelliteInput{}}
	for encodedID, version := range versions {
		parsed, err := strconv.ParseUint(encodedID, 10, 64)
		if err != nil {
			return dto.FrozenInputs{}, Internal("stored window ID is invalid", err)
		}
		frozen.Windows = append(frozen.Windows, dto.FrozenWindowInput{ID: uint(parsed), Version: version})
	}
	sort.Slice(frozen.Windows, func(i, j int) bool { return frozen.Windows[i].ID < frozen.Windows[j].ID })
	return frozen, nil
}

// diffFrozenInputs compares the frozen snapshot with the current database
// state and lists every changed object with its before/after values.
func diffFrozenInputs(frozen dto.FrozenInputs, windows map[uint]model.ContactWindow, stations map[uint]model.GroundStation, satellites map[uint]model.SatelliteAsset) []dto.FreezeViolation {
	violations := []dto.FreezeViolation{}
	for _, frozenWindow := range frozen.Windows {
		label := windowLabel(frozenWindow.ID)
		window, ok := windows[frozenWindow.ID]
		if !ok {
			violations = append(violations, freezeViolation(constants.FreezeObjectWindow, frozenWindow.ID, label, "object", "present", "missing"))
			continue
		}
		if window.Version == frozenWindow.Version {
			continue
		}
		if frozenWindow.WindowStatus == "" {
			violations = append(violations, freezeViolation(constants.FreezeObjectWindow, window.ID, label, "version", strconvUint(frozenWindow.Version), strconvUint(window.Version)))
			continue
		}
		changed := false
		if window.Band != frozenWindow.Band {
			violations = append(violations, freezeViolation(constants.FreezeObjectWindow, window.ID, label, "band", frozenWindow.Band, window.Band))
			changed = true
		}
		if window.WindowStatus != frozenWindow.WindowStatus {
			violations = append(violations, freezeViolation(constants.FreezeObjectWindow, window.ID, label, "status", frozenWindow.WindowStatus, window.WindowStatus))
			changed = true
		}
		if window.Priority != frozenWindow.Priority {
			violations = append(violations, freezeViolation(constants.FreezeObjectWindow, window.ID, label, "priority", strconv.Itoa(frozenWindow.Priority), strconv.Itoa(window.Priority)))
			changed = true
		}
		if !changed {
			violations = append(violations, freezeViolation(constants.FreezeObjectWindow, window.ID, label, "version", strconvUint(frozenWindow.Version), strconvUint(window.Version)))
		}
	}
	for _, frozenStation := range frozen.Stations {
		station, ok := stations[frozenStation.ID]
		if !ok {
			violations = append(violations, freezeViolation(constants.FreezeObjectStation, frozenStation.ID, stationLabel(frozenStation.Code, frozenStation.ID), "object", "present", "missing"))
			continue
		}
		label := stationLabel(station.StationCode, station.ID)
		if station.AntennaCount != frozenStation.AntennaCount {
			violations = append(violations, freezeViolation(constants.FreezeObjectStation, station.ID, label, "capacity", strconv.Itoa(frozenStation.AntennaCount), strconv.Itoa(station.AntennaCount)))
		}
		if current := decodeBands(station.SupportedBandsJSON); !equalStrings(current, frozenStation.SupportedBands) {
			violations = append(violations, freezeViolation(constants.FreezeObjectStation, station.ID, label, "band", strings.Join(frozenStation.SupportedBands, ","), strings.Join(current, ",")))
		}
		if station.StationStatus != frozenStation.StationStatus {
			violations = append(violations, freezeViolation(constants.FreezeObjectStation, station.ID, label, "status", frozenStation.StationStatus, station.StationStatus))
		}
	}
	for _, frozenSatellite := range frozen.Satellites {
		asset, ok := satellites[frozenSatellite.ID]
		if !ok {
			violations = append(violations, freezeViolation(constants.FreezeObjectSatellite, frozenSatellite.ID, satelliteLabel(frozenSatellite.Code, frozenSatellite.ID), "object", "present", "missing"))
			continue
		}
		label := satelliteLabel(asset.SatelliteCode, asset.ID)
		if current := decodeBands(asset.SupportedBandsJSON); !equalStrings(current, frozenSatellite.SupportedBands) {
			violations = append(violations, freezeViolation(constants.FreezeObjectSatellite, asset.ID, label, "band", strings.Join(frozenSatellite.SupportedBands, ","), strings.Join(current, ",")))
		}
		if asset.AssetStatus != frozenSatellite.AssetStatus {
			violations = append(violations, freezeViolation(constants.FreezeObjectSatellite, asset.ID, label, "status", frozenSatellite.AssetStatus, asset.AssetStatus))
		}
		if asset.PriorityWeight != frozenSatellite.PriorityWeight {
			violations = append(violations, freezeViolation(constants.FreezeObjectSatellite, asset.ID, label, "priority", strconv.FormatFloat(frozenSatellite.PriorityWeight, 'f', -1, 64), strconv.FormatFloat(asset.PriorityWeight, 'f', -1, 64)))
		}
		if asset.MinimumContactSec != frozenSatellite.MinimumContactSec {
			violations = append(violations, freezeViolation(constants.FreezeObjectSatellite, asset.ID, label, "minimum_contact", strconv.Itoa(frozenSatellite.MinimumContactSec), strconv.Itoa(asset.MinimumContactSec)))
		}
	}
	return violations
}

// checkFrozenInputs loads every frozen object with a row lock inside the
// review transaction and returns the current violations, if any.
func (service *ConflictResolutionService) checkFrozenInputs(tx *gorm.DB, resolution model.ConflictResolution) ([]dto.FreezeViolation, error) {
	frozen, err := parseFrozenInputs(resolution)
	if err != nil {
		return nil, err
	}
	windows := map[uint]model.ContactWindow{}
	for _, entry := range frozen.Windows {
		window, err := service.windows.FindForUpdate(tx, entry.ID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				continue
			}
			return nil, MapRepositoryError("contact window", err)
		}
		windows[window.ID] = window
	}
	stations := map[uint]model.GroundStation{}
	for _, entry := range frozen.Stations {
		station, err := service.stations.FindForUpdate(tx, entry.ID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				continue
			}
			return nil, MapRepositoryError("ground station", err)
		}
		stations[station.ID] = station
	}
	satellites := map[uint]model.SatelliteAsset{}
	for _, entry := range frozen.Satellites {
		asset, err := service.assets.FindForUpdate(tx, entry.ID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				continue
			}
			return nil, MapRepositoryError("satellite asset", err)
		}
		satellites[asset.ID] = asset
	}
	return diffFrozenInputs(frozen, windows, stations, satellites), nil
}

// attachFreezeViews computes the live freeze status for every resolution so a
// refreshed page always shows the current blocking reasons and before/after
// values without rewriting the stored snapshot.
func (service *ConflictResolutionService) attachFreezeViews(resolutions []model.ConflictResolution, responses []dto.ConflictResolutionResponse) error {
	frozens := make([]dto.FrozenInputs, 0, len(resolutions))
	windowIDs, stationIDs, satelliteIDs := map[uint]bool{}, map[uint]bool{}, map[uint]bool{}
	for _, resolution := range resolutions {
		frozen, err := parseFrozenInputs(resolution)
		if err != nil {
			return err
		}
		for _, entry := range frozen.Windows {
			windowIDs[entry.ID] = true
		}
		for _, entry := range frozen.Stations {
			stationIDs[entry.ID] = true
		}
		for _, entry := range frozen.Satellites {
			satelliteIDs[entry.ID] = true
		}
		frozens = append(frozens, frozen)
	}
	windows, err := service.windows.GetMany(sortedIDs(windowIDs))
	if err != nil {
		return Internal("could not load frozen windows", err)
	}
	stations, err := service.stations.GetMany(sortedIDs(stationIDs))
	if err != nil {
		return Internal("could not load frozen stations", err)
	}
	satellites, err := service.assets.GetMany(sortedIDs(satelliteIDs))
	if err != nil {
		return Internal("could not load frozen satellites", err)
	}
	windowMap := map[uint]model.ContactWindow{}
	for _, window := range windows {
		windowMap[window.ID] = window
	}
	stationMap := map[uint]model.GroundStation{}
	for _, station := range stations {
		stationMap[station.ID] = station
	}
	satelliteMap := map[uint]model.SatelliteAsset{}
	for _, asset := range satellites {
		satelliteMap[asset.ID] = asset
	}
	checkedAt := time.Now().UTC()
	for index := range responses {
		violations := diffFrozenInputs(frozens[index], windowMap, stationMap, satelliteMap)
		status := constants.FreezeStatusIntact
		if len(violations) > 0 {
			status = constants.FreezeStatusViolated
		}
		responses[index].Freeze = dto.FreezeStatusView{Status: status, FrozenAt: frozens[index].FrozenAt, CheckedAt: checkedAt, Violations: violations}
	}
	return nil
}

func freezeBlockMessage(violations []dto.FreezeViolation) string {
	labels := make([]string, 0, len(violations))
	for _, violation := range violations {
		labels = append(labels, fmt.Sprintf("%s %s %s", violation.ObjectType, violation.ObjectLabel, violation.Field))
	}
	shown := labels
	if len(shown) > 3 {
		shown = shown[:3]
	}
	message := "frozen planning inputs changed after detection: " + strings.Join(shown, ", ")
	if remaining := len(labels) - len(shown); remaining > 0 {
		message += fmt.Sprintf(" and %d more", remaining)
	}
	return message + "; accept is blocked until the inputs are restored or the conflict is re-scanned"
}

func freezeViolation(objectType string, objectID uint, label, field, frozenValue, currentValue string) dto.FreezeViolation {
	return dto.FreezeViolation{ObjectType: objectType, ObjectID: objectID, ObjectLabel: label, Field: field, FrozenValue: frozenValue, CurrentValue: currentValue}
}

func decodeBands(encoded string) []string {
	bands := []string{}
	if err := json.Unmarshal([]byte(encoded), &bands); err != nil {
		return []string{}
	}
	return bands
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func sortedIDs(set map[uint]bool) []uint {
	ids := make([]uint, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func windowLabel(id uint) string { return fmt.Sprintf("#%d", id) }
func stationLabel(code string, id uint) string {
	if code == "" {
		return fmt.Sprintf("#%d", id)
	}
	return code
}
func satelliteLabel(code string, id uint) string {
	if code == "" {
		return fmt.Sprintf("#%d", id)
	}
	return code
}
func strconvUint(value uint) string { return strconv.FormatUint(uint64(value), 10) }
