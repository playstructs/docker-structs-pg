package payload

// Permission matches structs.structs.EventPermission.permissionRecord.
// SQL handler: cache.handle_event_permission
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:1621-1681)
//
// `value` is a stringified integer. Empty string means DELETE.
//
// `permissionId` is a structured string parsed by the handler — see
// internal/events/permission.go for the decoder. Grammar:
//
//	{typeId}-{objectId}@{playerId}              -- non-address types
//	{typeId}-{address}                          -- type 8 (address)
//
// where typeId is one of 0..11 (see object_type table). For type 8,
// object_id and player_id are derived by looking up
// structs.player_address.player_id where address = the typeId's segment.
type Permission struct {
	PermissionID string `json:"permissionId"`
	Value        string `json:"value"`
}
