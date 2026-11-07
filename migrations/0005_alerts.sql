-- Events and the geofence (plan §7; guide step 5.6).

-- What happened to a drone, beyond its telemetry: alerts raised and cleared.
CREATE TABLE events (
    tenant_id bigint      NOT NULL,
    drone_id  bigint      NOT NULL,
    at        timestamptz NOT NULL,
    type      text        NOT NULL, -- 'alert'
    details   jsonb       NOT NULL
);

CREATE INDEX events_by_drone ON events (drone_id, at DESC);

-- One geofence per tenant in v1: the area drones must stay inside. It is
-- checked by the platform, not by ArduPilot (plan decision 11), so it works
-- for every drone.
CREATE TABLE geofences (
    tenant_id  bigint      PRIMARY KEY REFERENCES tenants (id),
    polygon    jsonb       NOT NULL, -- [{"lat":..,"lon":..}, ...]
    updated_by text        NOT NULL,
    updated_at timestamptz NOT NULL
);
