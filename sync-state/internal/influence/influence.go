// Package influence captures canonical raid/membership facts and drives the
// Sqitch-owned projector inside the existing block transaction. It never owns
// schema creation and never derives eligibility from a buffered activity feed.
package influence

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"sync-state/internal/payload"
	"sync-state/internal/rpc"
)

type Position struct {
	Chain      string    `json:"chain"`
	Height     int64     `json:"height"`
	Hash       string    `json:"hash"`
	Time       time.Time `json:"time"`
	Phase      int16     `json:"phase"`
	TxIndex    int       `json:"tx_index"`
	EventIndex int       `json:"event_index"`
	MsgIndex   int       `json:"msg_index"`
}
type theft struct {
	Attacker   string          `json:"thiefPlayerId"`
	Defender   string          `json:"victimPlayerId"`
	Amount     payload.Numeric `json:"amount"`
	EventIndex int             `json:"-"`
	Used       bool            `json:"-"`
}
type messageKey struct{ Tx, Msg int }
type Tracker struct {
	position Position
	enabled  bool
	thefts   map[messageKey][]*theft
}

func Begin(ctx context.Context, tx pgx.Tx, chain string, height int64, hash string, t time.Time, reconcile bool) (*Tracker, error) {
	tr := &Tracker{position: Position{Chain: chain, Height: height, Hash: hash, Time: t}, thefts: make(map[messageKey][]*theft)}
	if err := tx.QueryRow(ctx, `SELECT structs.influence_begin_block($1,$2,$3,$4,$5)`, chain, height, hash, t, reconcile).Scan(&tr.enabled); err != nil {
		return nil, fmt.Errorf("influence begin block: %w", err)
	}
	return tr, nil
}

func required(ev rpc.Event, key string) (json.RawMessage, error) {
	var result json.RawMessage
	count := 0
	for _, a := range ev.Attributes {
		if a.Key == key {
			result = json.RawMessage(a.Value)
			count++
		}
	}
	if count != 1 || !json.Valid(result) {
		return nil, fmt.Errorf("influence %s requires exactly one valid %s", ev.Type, key)
	}
	return result, nil
}
func amount(n payload.Numeric) (uint64, error) {
	// Proto3 omission means zero; explicit malformed/negative/fractional amounts fail.
	s := n.String()
	if s == "" {
		s = "0"
	}
	return strconv.ParseUint(s, 10, 64)
}

// Observe runs immediately before the existing handler for each successful
// transaction event (and for ordered block events). Any error aborts the block.
func (tr *Tracker) Observe(ctx context.Context, tx pgx.Tx, e rpc.PositionedEvent) error {
	if !tr.enabled {
		return nil
	}
	p := tr.position
	p.Phase = int16(e.Phase)
	p.TxIndex = e.TxIndex
	p.EventIndex = e.EventIndex
	p.MsgIndex = e.MsgIndex
	key := messageKey{e.TxIndex, e.MsgIndex}
	switch e.Event.Type {
	case "structs.structs.EventPlayer":
		raw, err := required(e.Event, "player")
		if err != nil {
			return err
		}
		v, err := payload.Decode[payload.Player](raw)
		if err != nil {
			return err
		}
		if v.ID == "" {
			return fmt.Errorf("influence player: empty id")
		}
		_, err = tx.Exec(ctx, `SELECT structs.influence_record_member($1,$2,$3)`, p, v.ID, v.GuildID)
		return err
	case "structs.structs.EventOreTheft":
		if e.Phase != rpc.Transaction || e.MsgIndex < 0 {
			return fmt.Errorf("influence theft lacks successful transaction/message identity")
		}
		raw, err := required(e.Event, "eventOreTheftDetail")
		if err != nil {
			return err
		}
		v, err := payload.Decode[theft](raw)
		if err != nil {
			return err
		}
		if v.Attacker == "" || v.Defender == "" {
			return fmt.Errorf("influence theft lacks player ids")
		}
		if _, err = amount(v.Amount); err != nil {
			return fmt.Errorf("influence theft amount: %w", err)
		}
		v.EventIndex = e.EventIndex
		tr.thefts[key] = append(tr.thefts[key], &v)
		return nil
	case "structs.structs.EventRaid":
		raw, err := required(e.Event, "eventRaidDetail")
		if err != nil {
			return err
		}
		raid, err := payload.Decode[payload.Raid](raw)
		if err != nil {
			return err
		}
		if raid.Status != "raidSuccessful" {
			return nil
		}
		if e.Phase != rpc.Transaction || e.MsgIndex < 0 {
			return fmt.Errorf("influence raid lacks successful transaction/message identity")
		}
		seized, err := amount(raid.SeizedOre)
		if err != nil {
			return fmt.Errorf("influence seized ore: %w", err)
		}
		var attacker, defender string
		var sector int16
		var attackerGuild, defenderGuild *string
		err = tx.QueryRow(ctx, `SELECT f.owner,p.owner,p.sector,a.guild_id,d.guild_id
   FROM structs.fleet f JOIN structs.player a ON a.id=f.owner
   CROSS JOIN structs.planet p JOIN structs.player d ON d.id=p.owner
   WHERE f.id=$1 AND p.id=$2`, raid.FleetID, raid.PlanetID).Scan(&attacker, &defender, &sector, &attackerGuild, &defenderGuild)
		if err != nil {
			return fmt.Errorf("influence raid attribution fleet=%s planet=%s: %w", raid.FleetID, raid.PlanetID, err)
		}
		var matched *theft
		for _, candidate := range tr.thefts[key] {
			q, err := amount(candidate.Amount)
			if err != nil {
				return err
			}
			if !candidate.Used && candidate.EventIndex < e.EventIndex && candidate.Attacker == attacker && candidate.Defender == defender && q == seized {
				if matched != nil {
					return fmt.Errorf("influence ambiguous theft/raid pairing")
				}
				matched = candidate
			}
		}
		if matched == nil {
			return fmt.Errorf("influence raid has no matching theft in its message")
		}
		facts := map[string]any{"attacker": attacker, "defender": defender, "attacker_guild": attackerGuild,
			"defender_guild": defenderGuild, "planet": raid.PlanetID, "fleet": raid.FleetID, "sector": sector,
			"seized_ore": strconv.FormatUint(seized, 10), "theft_event_index": matched.EventIndex}
		_, err = tx.Exec(ctx, `SELECT structs.influence_record_raid($1,$2)`, p, facts)
		if err == nil {
			matched.Used = true
		}
		return err
	}
	return nil
}
func (tr *Tracker) Finish(ctx context.Context, tx pgx.Tx) error {
	if !tr.enabled {
		return nil
	}
	for key, rows := range tr.thefts {
		for _, t := range rows {
			if !t.Used {
				return fmt.Errorf("influence unmatched theft tx=%d msg=%d event=%d", key.Tx, key.Msg, t.EventIndex)
			}
		}
	}
	_, err := tx.Exec(ctx, `SELECT structs.influence_finish_block($1,$2,$3)`, tr.position.Chain, tr.position.Height, tr.position.Hash)
	return err
}

func ValidateSchema(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) error {
	var ok bool
	err := q.QueryRow(ctx, `SELECT
 to_regprocedure('structs.influence_begin_block(text,bigint,text,timestamptz,boolean)') IS NOT NULL
 AND to_regprocedure('structs.influence_record_raid(jsonb,jsonb)') IS NOT NULL
 AND to_regprocedure('structs.influence_record_member(jsonb,text,text)') IS NOT NULL
 AND to_regprocedure('structs.influence_finish_block(text,bigint,text)') IS NOT NULL
 AND to_regprocedure('structs.influence_rebuild(bigint)') IS NOT NULL
 AND to_regprocedure('structs.influence_reconcile(bigint)') IS NOT NULL
 AND to_regclass('structs.influence_score_change_1000') IS NOT NULL
 AND EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='structs' AND table_name='influence_membership_history' AND column_name='event_time')
 AND EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='structs' AND table_name='influence_raid' AND column_name='source')`).Scan(&ok)
	if err != nil {
		return fmt.Errorf("validate influence schema: %w", err)
	}
	if !ok {
		return fmt.Errorf("apply structs-pg influence-schema-20261111 and influence-projector-20261111 before this writer")
	}
	return nil
}
