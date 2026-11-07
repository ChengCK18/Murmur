-- Missions (plan §7): the waypoints sent to a drone and whether they arrived.
-- A drone's newest row is the mission it holds, if that row is 'uploaded'.

CREATE TABLE missions (
    id         uuid        PRIMARY KEY, -- the upload's command ID
    tenant_id  bigint      NOT NULL REFERENCES tenants (id),
    drone_id   bigint      NOT NULL,
    waypoints  jsonb       NOT NULL,
    altitude_m real        NOT NULL,
    status     text        NOT NULL,    -- 'uploading', 'uploaded' or 'failed'
    issued_by  text        NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

CREATE INDEX missions_by_drone ON missions (drone_id, created_at DESC);
