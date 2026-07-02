package query

// Document is the backend-neutral shape of a composed read-model document.
// It is a plain map alias (identical to the Mongo driver's bson.M underlying
// type, map[string]interface{}), so a value flows between the composer, the
// SyncEngine and the store without a conversion and without the generic
// read-model code importing the Mongo driver. The Mongo adapter passes it
// straight to the driver, which marshals a map[string]any as a BSON document.
type Document = map[string]any
