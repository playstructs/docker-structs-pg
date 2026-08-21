// Package readmodel maintains the indexer-owned structs.api_* current-state
// projections. Dirty is the per-block dependency set shared by event handlers.
package readmodel

import "sync-state/internal/buffers"

// Dirty records authoritative entities whose projections must be recomputed.
// Maps are used so repeated events in one block remain bounded and idempotent.
type Dirty struct {
	Addresses   map[string]struct{}
	Players     map[string]struct{}
	Guilds      map[string]struct{}
	Reactors    map[string]struct{}
	Providers   map[string]struct{}
	Substations map[string]struct{}
	GridObjects map[string]struct{}
}

func NewDirty() *Dirty {
	return &Dirty{
		Addresses:   map[string]struct{}{},
		Players:     map[string]struct{}{},
		Guilds:      map[string]struct{}{},
		Reactors:    map[string]struct{}{},
		Providers:   map[string]struct{}{},
		Substations: map[string]struct{}{},
		GridObjects: map[string]struct{}{},
	}
}

func add(m map[string]struct{}, id string) {
	if id != "" {
		m[id] = struct{}{}
	}
}

func (d *Dirty) Address(id string) {
	if d != nil {
		add(d.Addresses, id)
	}
}
func (d *Dirty) Player(id string) {
	if d != nil {
		add(d.Players, id)
	}
}
func (d *Dirty) Guild(id string) {
	if d != nil {
		add(d.Guilds, id)
	}
}
func (d *Dirty) Reactor(id string) {
	if d != nil {
		add(d.Reactors, id)
	}
}
func (d *Dirty) Provider(id string) {
	if d != nil {
		add(d.Providers, id)
	}
}
func (d *Dirty) Substation(id string) {
	if d != nil {
		add(d.Substations, id)
	}
}
func (d *Dirty) GridObject(id string) {
	if d != nil {
		add(d.GridObjects, id)
	}
}

// Ledger marks every address touched by buffered ledger rows and guilds whose
// token supply changed. Call before Buffer.Flush resets the ledger slice.
func (d *Dirty) Ledger(rows []buffers.LedgerRow) {
	if d == nil {
		return
	}
	for _, row := range rows {
		d.Address(row.Address)
		const prefix = "uguild."
		if len(row.Denom) > len(prefix) && row.Denom[:len(prefix)] == prefix &&
			(row.Action == "minted" || row.Action == "burned") {
			d.Guild(row.Denom[len(prefix):])
		}
	}
}

type Snapshot struct{ value *Dirty }

func cloneSet(src map[string]struct{}) map[string]struct{} {
	dst := make(map[string]struct{}, len(src))
	for k := range src {
		dst[k] = struct{}{}
	}
	return dst
}

func (d *Dirty) Snapshot() Snapshot {
	if d == nil {
		return Snapshot{}
	}
	return Snapshot{value: &Dirty{
		Addresses: cloneSet(d.Addresses), Players: cloneSet(d.Players),
		Guilds: cloneSet(d.Guilds), Reactors: cloneSet(d.Reactors),
		Providers: cloneSet(d.Providers), Substations: cloneSet(d.Substations),
		GridObjects: cloneSet(d.GridObjects),
	}}
}

func (d *Dirty) Restore(s Snapshot) {
	if d == nil || s.value == nil {
		return
	}
	*d = *s.value
}

func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func (d *Dirty) AddressIDs() []string    { return keys(d.Addresses) }
func (d *Dirty) PlayerIDs() []string     { return keys(d.Players) }
func (d *Dirty) GuildIDs() []string      { return keys(d.Guilds) }
func (d *Dirty) ReactorIDs() []string    { return keys(d.Reactors) }
func (d *Dirty) ProviderIDs() []string   { return keys(d.Providers) }
func (d *Dirty) SubstationIDs() []string { return keys(d.Substations) }
func (d *Dirty) GridObjectIDs() []string { return keys(d.GridObjects) }
