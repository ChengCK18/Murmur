package db

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ChengCK18/murmur/internal/registry"
)

// Drones is the drones table as a registry.Store, for one tenant.
type Drones struct {
	Pool     *pgxpool.Pool
	TenantID int64
}

var _ registry.Store = Drones{}

func (s Drones) Load(ctx context.Context) ([]registry.Drone, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, src_addr, sys_id, first_seen, last_seen
		FROM drones WHERE tenant_id = $1 ORDER BY id`, s.TenantID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (registry.Drone, error) {
		var d registry.Drone
		var src string
		var sysID int16
		if err := row.Scan(&d.ID, &src, &sysID, &d.FirstSeen, &d.LastHeartbeat); err != nil {
			return d, err
		}
		var err error
		if d.Src, err = netip.ParseAddrPort(src); err != nil {
			return d, fmt.Errorf("drone %d: %w", d.ID, err)
		}
		d.SysID = uint8(sysID)
		return d, nil
	})
}

// Create stores a new drone. If the table already has one at that address and
// system ID, its ID is returned, so a lost reply cannot make two rows.
func (s Drones) Create(ctx context.Context, d registry.Drone) (registry.ID, error) {
	var id registry.ID
	err := s.Pool.QueryRow(ctx, `INSERT INTO drones (tenant_id, src_addr, sys_id, first_seen, last_seen)
		VALUES ($1, $2, $3, $4, $4)
		ON CONFLICT (src_addr, sys_id) DO UPDATE SET last_seen = excluded.last_seen
		RETURNING id`, s.TenantID, d.Src.String(), int16(d.SysID), d.LastHeartbeat).Scan(&id)
	return id, err
}

func (s Drones) Move(ctx context.Context, d registry.Drone) error {
	_, err := s.Pool.Exec(ctx, `UPDATE drones SET src_addr = $2, last_seen = $3 WHERE id = $1`,
		int64(d.ID), d.Src.String(), d.LastHeartbeat)
	return err
}
