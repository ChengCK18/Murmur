-- The command audit log (plan §4, §7; guide step 5.1): every command with its
-- user, times, attempts and result, whether or not it reached the drone.

CREATE TABLE commands (
    id         uuid        PRIMARY KEY,
    tenant_id  bigint      NOT NULL REFERENCES tenants (id),
    drone_id   bigint      NOT NULL,
    type       text        NOT NULL,
    params     jsonb       NOT NULL,
    issued_by  text        NOT NULL,
    issued_at  timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    status     text        NOT NULL,
    reason     text,                 -- why it was refused or failed
    attempts   integer     NOT NULL DEFAULT 0,
    mav_result smallint,             -- MAV_RESULT from COMMAND_ACK
    updated_at timestamptz NOT NULL
);

CREATE INDEX commands_by_drone ON commands (drone_id, issued_at DESC);
