package dto

import "time"

type DetectConflictsRequest struct {
	From string `json:"from" validate:"required"`
	To   string `json:"to" validate:"required"`
}

type ConflictActionRequest struct {
	ExpectedVersion uint   `json:"expected_version" validate:"required,gte=1"`
	ActionKey       string `json:"action_key" validate:"omitempty,max=120"`
	Decision        string `json:"decision" validate:"omitempty,oneof=accepted rejected"`
	ReviewNote      string `json:"review_note" validate:"omitempty,max=500"`
}

type ScoreBreakdown struct {
	PriorityLoss       float64 `json:"priority_loss"`
	MovementDistanceKM float64 `json:"movement_distance_km"`
	ContactDurationSec int     `json:"contact_duration_sec"`
	ResourceMargin     int     `json:"resource_margin"`
	TotalScore         float64 `json:"total_score"`
}

type ResolutionSuggestion struct {
	ActionKey         string         `json:"action_key"`
	ActionType        string         `json:"action_type"`
	Title             string         `json:"title"`
	Rationale         string         `json:"rationale"`
	KeepWindowIDs     []uint         `json:"keep_window_ids"`
	MoveWindowIDs     []uint         `json:"move_window_ids"`
	TargetStationID   *uint          `json:"target_station_id,omitempty"`
	AlternateWindowID *uint          `json:"alternate_window_id,omitempty"`
	RequiresManual    bool           `json:"requires_manual"`
	Score             ScoreBreakdown `json:"score"`
}

type ConflictEvidence struct {
	Summary         string                 `json:"summary"`
	WindowFacts     []map[string]any       `json:"window_facts"`
	Capacity        int                    `json:"capacity"`
	PeakConcurrency int                    `json:"peak_concurrency"`
	BufferSeconds   int                    `json:"buffer_seconds"`
	Metadata        map[string]interface{} `json:"metadata"`
}

type FrozenWindowInput struct {
	ID           uint   `json:"id"`
	Version      uint   `json:"version"`
	WindowStatus string `json:"window_status"`
	Band         string `json:"band"`
	Priority     int    `json:"priority"`
}

type FrozenStationInput struct {
	ID             uint     `json:"id"`
	Version        uint     `json:"version"`
	StationCode    string   `json:"station_code"`
	AntennaCount   int      `json:"antenna_count"`
	SupportedBands []string `json:"supported_bands"`
	StationStatus  string   `json:"station_status"`
}

type FrozenSatelliteInput struct {
	ID                uint     `json:"id"`
	Version           uint     `json:"version"`
	SatelliteCode     string   `json:"satellite_code"`
	SupportedBands    []string `json:"supported_bands"`
	PriorityWeight    float64  `json:"priority_weight"`
	MinimumContactSec int      `json:"minimum_contact_sec"`
	AssetStatus       string   `json:"asset_status"`
}

type FrozenInputsSnapshot struct {
	FrozenAt   time.Time              `json:"frozen_at"`
	Windows    []FrozenWindowInput    `json:"windows"`
	Stations   []FrozenStationInput   `json:"stations"`
	Satellites []FrozenSatelliteInput `json:"satellites"`
}

type FrozenInputChange struct {
	ObjectType string `json:"object_type"`
	ObjectID   uint   `json:"object_id"`
	Label      string `json:"label"`
	Field      string `json:"field"`
	Before     any    `json:"before"`
	After      any    `json:"after"`
}

type ConflictResolutionResponse struct {
	ID                  uint                   `json:"id"`
	ConflictKey         string                 `json:"conflict_key"`
	WindowIDs           []uint                 `json:"window_ids"`
	ConflictType        string                 `json:"conflict_type"`
	Evidence            ConflictEvidence       `json:"evidence"`
	Suggestions         []ResolutionSuggestion `json:"suggestions"`
	SelectedAction      *ResolutionSuggestion  `json:"selected_action,omitempty"`
	ResolutionStatus    string                 `json:"resolution_status"`
	ResolvedBy          string                 `json:"resolved_by"`
	ReviewNote          string                 `json:"review_note"`
	FreezeStatus        string                 `json:"freeze_status"`
	FreezeBlockedReason string                 `json:"freeze_blocked_reason,omitempty"`
	FrozenInputs        *FrozenInputsSnapshot  `json:"frozen_inputs,omitempty"`
	FrozenChanges       []FrozenInputChange    `json:"frozen_changes"`
	Version             uint                   `json:"version"`
	ResolvedAt          *time.Time             `json:"resolved_at,omitempty"`
	CreatedAt           time.Time              `json:"created_at"`
	UpdatedAt           time.Time              `json:"updated_at"`
}

type DetectionResult struct {
	RangeFrom     time.Time                    `json:"range_from"`
	RangeTo       time.Time                    `json:"range_to"`
	WindowCount   int                          `json:"window_count"`
	ConflictCount int                          `json:"conflict_count"`
	Resolutions   []ConflictResolutionResponse `json:"resolutions"`
}
