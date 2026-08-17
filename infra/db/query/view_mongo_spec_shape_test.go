package query

import (
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type shapeRoot struct {
	ID    string
	Email string
	Name  string
}

type shapeChild struct {
	ID      string
	ZipCode string
}

// shapeView builds a realistic view: root users(id,email,name + managed
// cols) with an embedded addresses(id, user_id ParentID, zip_code) collection.
func shapeView() *ViewDefinition {
	root := core.NewTableSchema[shapeRoot]("users").
		ID("id").
		Field("Email", "email").
		Field("Name", "name").
		DeletedAt("deleted_at").
		CreatedAt("created_at").
		UpdatedAt("updated_at")
	child := core.NewExternalSchema("addresses").
		ID("id").
		Field("ZipCode", "zip_code")
	return View("users").Version(1).
		Schema(root).
		EmbedMany(JoinUpstream(child, "Addresses", "addresses")).On("user_id")
}

func TestValidateMongoSpec_IndexKey_ValidColumns_OK(t *testing.T) {
	v := shapeView().Indexes(
		Index("email"),
		Index("created_at").Desc(),  // managed column
		Index("addresses.zip_code"), // embed nested column
		Index("addresses.user_id"),  // embed ParentID column
		Compound("email", "name"),
		TextIndex("name", "email"),
	)
	if err := v.ValidateMongoSpec(); err != nil {
		t.Fatalf("every key is an emitted column; want nil, got: %v", err)
	}
}

func TestValidateMongoSpec_IndexKey_Typo_Rejected(t *testing.T) {
	v := shapeView().Indexes(Index("emial"))
	err := v.ValidateMongoSpec()
	if err == nil {
		t.Fatal("expected error for an index on a non-emitted column")
	}
	if !strings.Contains(err.Error(), "emial") || !strings.Contains(err.Error(), "never be used") {
		t.Errorf("error must name the dead key, got: %v", err)
	}
}

func TestValidateMongoSpec_IndexKey_GoNameNotColumn_Rejected(t *testing.T) {
	// The composed doc carries "zip_code" (physical), not "zipCode" (Go).
	v := shapeView().Indexes(Index("addresses.zipCode"))
	if err := v.ValidateMongoSpec(); err == nil {
		t.Fatal("expected error: a Go field name is not a physical column path")
	}
}

func TestValidateMongoSpec_TextIndex_DeadKey_Rejected(t *testing.T) {
	v := shapeView().Indexes(TextIndex("name", "bogus"))
	if err := v.ValidateMongoSpec(); err == nil {
		t.Fatal("expected error: text index on a non-emitted column")
	}
}

func TestValidateMongoSpec_JSONSchemaRequired_Valid_OK(t *testing.T) {
	v := shapeView().JSONSchema(bson.M{
		"bsonType": "object",
		"required": []string{"_id", "email"},
	})
	if err := v.ValidateMongoSpec(); err != nil {
		t.Fatalf("required names emitted columns; want nil, got: %v", err)
	}
}

func TestValidateMongoSpec_JSONSchemaRequired_DeadField_Rejected(t *testing.T) {
	v := shapeView().JSONSchema(bson.M{
		"bsonType": "object",
		"required": []string{"email", "mail"}, // "mail" is not emitted
	})
	err := v.ValidateMongoSpec()
	if err == nil {
		t.Fatal("expected error: required entry on a non-emitted column")
	}
	if !strings.Contains(err.Error(), "mail") || !strings.Contains(err.Error(), "rejected") {
		t.Errorf("error must name the offending required field, got: %v", err)
	}
}

func TestValidateMongoSpec_JSONSchemaRequired_BsonA_OK(t *testing.T) {
	// bson.A is the shape a YAML/bson decode produces, not []string.
	v := shapeView().JSONSchema(bson.M{
		"required": bson.A{"email", "name"},
	})
	if err := v.ValidateMongoSpec(); err != nil {
		t.Fatalf("bson.A required must be honored; got: %v", err)
	}
}

func TestValidateMongoSpec_NoSchema_ShapeGuardSkipped(t *testing.T) {
	// A schema-less view (only built in isolation) has no column set to
	// compare against — the shape guard must not fire.
	v := View("anon").Version(1).Indexes(Index("whatever"))
	if err := v.ValidateMongoSpec(); err != nil {
		t.Fatalf("shape guard must be skipped without a schema; got: %v", err)
	}
}

// A SharedBase role's document carries the base's columns (mergeSharedBase) and
// its siblings' columns (mergeOwnerSiblings) FLAT — the emitted-column set the
// shape guard checks against must include them, matching what the composer
// emits. Regression for the gap where collectComposedColumns only knew the
// role's own columns and rejected a legitimate index on a base/sibling field.
type sbShapeRole struct {
	ID         string
	Document   string
	Name       string
	Email      string
	UserName   string
	EmailNotif *bool
}

func sbShapeView() *ViewDefinition {
	base := core.NewSharedBaseSchema("persons").Revision("revision").
		ID("id").
		Field("Document", "document").
		Field("Name", "name").
		Field("Email", "email").
		NaturalID("document")
	role := core.NewTableSchema[sbShapeRole]("users").
		ID("id").
		SharedBase(base, "person_id").
		Field("UserName", "user_name").
		Sibling(core.NewSiblingSchema[sbShapeRole]("user_configurations").
			Field("EmailNotif", "email_notification"))
	return View("users").Version(1).Schema(role)
}

func TestValidateMongoSpec_IndexKey_SharedBaseAndSiblingColumns_OK(t *testing.T) {
	v := sbShapeView().Indexes(
		Index("document"),           // SharedBase natural-key column
		Index("email"),              // SharedBase column
		Index("email_notification"), // Sibling column
		Index("user_name"),          // role's own column
		TextIndex("name", "email"),  // TextIndex over SharedBase columns
	)
	if err := v.ValidateMongoSpec(); err != nil {
		t.Fatalf("index keys on SharedBase/Sibling columns are emitted; want nil, got: %v", err)
	}
}

func TestValidateMongoSpec_IndexKey_SharedBase_DeadKeyStillRejected(t *testing.T) {
	// The base/sibling awareness must not turn the guard off: a truly-absent
	// column is still rejected.
	v := sbShapeView().Indexes(Index("cpf"))
	if err := v.ValidateMongoSpec(); err == nil {
		t.Fatal("expected error: 'cpf' is not a column any level emits")
	}
}
