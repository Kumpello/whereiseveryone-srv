package id

import (
	"encoding/json"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestIDWireCompatibility(t *testing.T) {
	const hex = "507f1f77bcf86cd799439011"
	value, err := FromString(hex)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `"`+hex+`"` {
		t.Fatalf("JSON ID changed: %s", encoded)
	}
	var decoded ID
	if err = json.Unmarshal(encoded, &decoded); err != nil || decoded != value {
		t.Fatalf("JSON round trip: %v", err)
	}
	data, err := bson.Marshal(struct {
		ID ID `bson:"_id"` //nolint:tagliatelle // MongoDB primary key
	}{value})
	if err != nil {
		t.Fatal(err)
	}
	objectID, ok := bson.Raw(data).Lookup("_id").ObjectIDOK()
	if !ok || objectID.Hex() != hex {
		t.Fatal("database ID must remain a BSON ObjectID")
	}
}
