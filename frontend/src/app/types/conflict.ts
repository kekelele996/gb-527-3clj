export type ConflictType = 'station_capacity' | 'satellite_overlap' | 'band_mismatch' | 'duration_shortfall' | 'slew_buffer';
export type ResolutionStatus = 'detected' | 'proposed' | 'pending_review' | 'accepted' | 'rejected';
export type FreezeStatus = 'frozen' | 'invalidated';
export type FrozenObjectType = 'contact_window' | 'ground_station' | 'satellite_asset';

export const CONFLICT_TYPES: ConflictType[] = ['station_capacity', 'satellite_overlap', 'band_mismatch', 'duration_shortfall', 'slew_buffer'];

export interface ScoreBreakdown {
  priority_loss: number;
  movement_distance_km: number;
  contact_duration_sec: number;
  resource_margin: number;
  total_score: number;
}

export interface ResolutionSuggestion {
  action_key: string;
  action_type: string;
  title: string;
  rationale: string;
  keep_window_ids: number[];
  move_window_ids: number[];
  target_station_id?: number;
  alternate_window_id?: number;
  requires_manual: boolean;
  score: ScoreBreakdown;
}

export interface ConflictEvidence {
  summary: string;
  window_facts: Array<Record<string, unknown>>;
  capacity: number;
  peak_concurrency: number;
  buffer_seconds: number;
  metadata: Record<string, unknown>;
}

export interface FrozenWindowInput {
  id: number;
  version: number;
  window_status: string;
  band: string;
  priority: number;
}

export interface FrozenStationInput {
  id: number;
  version: number;
  station_code: string;
  antenna_count: number;
  supported_bands: string[];
  station_status: string;
}

export interface FrozenSatelliteInput {
  id: number;
  version: number;
  satellite_code: string;
  supported_bands: string[];
  priority_weight: number;
  minimum_contact_sec: number;
  asset_status: string;
}

export interface FrozenInputsSnapshot {
  frozen_at: string;
  windows: FrozenWindowInput[];
  stations: FrozenStationInput[];
  satellites: FrozenSatelliteInput[];
}

export interface FrozenInputChange {
  object_type: FrozenObjectType;
  object_id: number;
  label: string;
  field: string;
  before: unknown;
  after: unknown;
}

export interface ConflictResolution {
  id: number;
  conflict_key: string;
  window_ids: number[];
  conflict_type: ConflictType;
  evidence: ConflictEvidence;
  suggestions: ResolutionSuggestion[];
  selected_action?: ResolutionSuggestion;
  resolution_status: ResolutionStatus;
  resolved_by: string;
  review_note: string;
  freeze_status: FreezeStatus;
  freeze_blocked_reason?: string;
  frozen_inputs?: FrozenInputsSnapshot;
  frozen_changes: FrozenInputChange[];
  version: number;
  resolved_at?: string;
  created_at: string;
  updated_at: string;
}

export interface DetectionResult {
  range_from: string;
  range_to: string;
  window_count: number;
  conflict_count: number;
  resolutions: ConflictResolution[];
}
