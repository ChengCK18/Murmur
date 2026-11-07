-- Commands sent to several drones at once (plan §4, "Fleet commands"): one row
-- per drone, tracked on its own, tied together by the fleet ID.

ALTER TABLE commands ADD COLUMN fleet_id uuid;

CREATE INDEX commands_by_fleet ON commands (fleet_id) WHERE fleet_id IS NOT NULL;
