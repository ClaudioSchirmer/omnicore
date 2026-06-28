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
// cols) with an embedded addresses(id, user_id FK, zip_code) collection.
func shapeView() *ViewDefinition {
	root := core.NewTableSchema[shapeRoot]("users").
		PK("id").
		Field("Email", "email").
		Field("Name", "name").
		SoftDelete("deleted_at").
		CreatedAt("created_at").
		UpdatedAt("updated_at")
	child := core.NewTableSchema[shapeChild]("addresses").
		PK("id").
		FK("user_id").
		Field("ZipCode", "zip_code")
	return View("users").Version(1).Root("users").
		Schema(root).
		EmbedMany("addresses", FromSchema(child))
}

func TestValidateMongoSpec_IndexKey_ValidColumns_OK(t *testing.T) {
	v := shapeView().Indexes(
		Index("email"),
		Index("created_at").Desc(),  // managed column
		Index("addresses.zip_code"), // embed nested column
		Index("addresses.user_id"),  // embed FK column
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
	v := View("anon").Version(1).Root("anon").Indexes(Index("whatever"))
	if err := v.ValidateMongoSpec(); err != nil {
		t.Fatalf("shape guard must be skipped without a schema; got: %v", err)
	}
}
