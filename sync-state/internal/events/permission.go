package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/payload"
)

// permissionHandler ports cache.handle_event_permission
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:1621-1681).
//
// Two branches keyed on payload.value:
//   - value == ""   → DELETE structs.permission WHERE id = permissionId
//   - else          → INSERT/UPDATE structs.permission with decoded id
//
// The permissionId is a structured string that the handler decodes into
// (object_type, object_index, object_id, player_id). Grammar:
//
//	{typeId}-{rest}
//	typeId ∈ {0..11}    -- maps to object_type label (see objectType labels)
//
// For typeId != 8:
//
//	rest = "{objectId}@{playerId}"        e.g. "5-42@1-1"
//	object_index = first @-segment of `rest`, stripped of the type prefix
//	object_id    = "{typeId}-{objectIndex}"  (i.e. first @-segment of the
//	               full permissionId, which already includes the typeId)
//	player_id    = second @-segment of the full permissionId
//
// For typeId == 8 (address):
//
//	rest = "{address}"                   e.g. "8-structs1abc..."
//	object_index = the address string
//	object_id    = lookup player_address.player_id WHERE address = rest
//	player_id    = same lookup as object_id
//
// On UPDATE, only `val` is refreshed (mirrors SQL: decode columns are
// INSERT-only). IS DISTINCT FROM guard on `val` suppresses no-op writes.
type permissionHandler struct{}

func (permissionHandler) CompositeKey() string {
	return "structs.structs.EventPermission.permissionRecord"
}

const permissionDeleteSQL = `DELETE FROM structs.permission WHERE id = $1`

const permissionUpsertSQL = `
INSERT INTO structs.permission (
    id, object_type, object_index, object_id, player_id, val, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, NOW())
ON CONFLICT (id) DO UPDATE
   SET val        = EXCLUDED.val,
       updated_at = NOW()
 WHERE structs.permission.val IS DISTINCT FROM EXCLUDED.val`

const permissionAddressLookupSQL = `SELECT player_id FROM structs.player_address WHERE address = $1`

func (permissionHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.Permission](raw)
	if err != nil {
		return err
	}
	if p.PermissionID == "" {
		return fmt.Errorf("permission: empty permissionId")
	}
	if p.Value == "" {
		if _, err := tx.Exec(ctx, permissionDeleteSQL, p.PermissionID); err != nil {
			return fmt.Errorf("permission delete id=%s: %w", p.PermissionID, err)
		}
		return nil
	}

	val, err := strconv.Atoi(p.Value)
	if err != nil {
		return fmt.Errorf("permission: invalid val %q for id=%s: %w", p.Value, p.PermissionID, err)
	}
	decoded, err := decodePermissionID(p.PermissionID)
	if err != nil {
		return fmt.Errorf("permission id=%s: %w", p.PermissionID, err)
	}

	// For type 8 (address) object_id and player_id come from the
	// player_address mapping. Look up once and bind to both.
	objectID := decoded.objectIDLiteral
	playerID := decoded.playerIDLiteral
	if decoded.typeID == addressTypeID {
		var resolved string
		err := tx.QueryRow(ctx, permissionAddressLookupSQL, decoded.objectIndex).Scan(&resolved)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("permission address lookup addr=%s: %w", decoded.objectIndex, err)
		}
		// ErrNoRows is fine; matches the SQL handler's "(SELECT ...) returns NULL"
		// — both columns end up NULL in the inserted row.
		objectID = resolved
		playerID = resolved
	}

	if _, err := tx.Exec(ctx, permissionUpsertSQL,
		p.PermissionID,
		decoded.objectType,
		decoded.objectIndex,
		nullIfEmpty(objectID),
		nullIfEmpty(playerID),
		val,
	); err != nil {
		return fmt.Errorf("permission upsert id=%s: %w", p.PermissionID, err)
	}
	return nil
}

// addressTypeID is the type prefix in permissionId for address-scoped
// permissions. Special-cased because object_id/player_id are resolved
// via player_address rather than being parsed from the string.
const addressTypeID = "8"

// permissionTypeLabels maps the type prefix to the object_type column
// value. Same mapping the SQL CASE uses.
var permissionTypeLabels = map[string]string{
	"0":  "guild",
	"1":  "player",
	"2":  "planet",
	"3":  "reactor",
	"4":  "substation",
	"5":  "struct",
	"6":  "allocation",
	"7":  "infusion",
	"8":  "address",
	"9":  "fleet",
	"10": "provider",
	"11": "agreement",
}

// decodedPermission is everything we need to write a structs.permission
// row from a permissionId string (plus the addressTypeID branch for
// object_id/player_id which is resolved against player_address).
type decodedPermission struct {
	typeID          string
	objectType      string
	objectIndex     string
	objectIDLiteral string
	playerIDLiteral string
}

// decodePermissionID parses a permissionId into its constituent parts,
// matching the SQL handler's split_part-based decode byte-for-byte.
func decodePermissionID(id string) (decodedPermission, error) {
	if id == "" {
		return decodedPermission{}, errors.New("empty permissionId")
	}
	d := decodedPermission{
		typeID: splitPart(id, "-", 1),
	}
	label, ok := permissionTypeLabels[d.typeID]
	if !ok {
		// SQL CASE returns NULL for unknown prefixes; we mirror that by
		// leaving object_type empty (becomes SQL NULL on insert).
		label = ""
	}
	d.objectType = label

	// object_index = split_part(split_part(id, '-', 2), '@', 1)
	d.objectIndex = splitPart(splitPart(id, "-", 2), "@", 1)

	// For non-address types, object_id and player_id come from the @-segments
	// of the full id. For address types, they're resolved by the caller via
	// a player_address lookup.
	d.objectIDLiteral = splitPart(id, "@", 1)
	d.playerIDLiteral = splitPart(id, "@", 2)

	return d, nil
}

// splitPart mirrors PostgreSQL's split_part(str, sep, n) function:
// returns the n-th (1-indexed) field of str split on sep. Returns the
// empty string when n is out of range (matches PG behavior).
func splitPart(s, sep string, n int) string {
	if n < 1 || s == "" {
		return ""
	}
	parts := strings.Split(s, sep)
	if n > len(parts) {
		return ""
	}
	return parts[n-1]
}

// nullIfEmpty is a local helper — using payload.NullableText would be
// fine but its current implementation passes empty strings through.
// The permission table accepts empty strings, but the SQL handler's
// CASE returns NULL for the type-8 not-found path (sub-SELECT returns
// NULL), and we want to match that exactly.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
