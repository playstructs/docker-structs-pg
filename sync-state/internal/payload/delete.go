package payload

// Delete matches structs.structs.EventDelete.objectId (structsd v0.21.0).
// The attribute value is either a typed object id or a JSON object
// wrapping that id.
type Delete struct {
	ObjectID string `json:"objectId"`
}
