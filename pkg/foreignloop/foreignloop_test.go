package foreignloop

import "testing"

func TestPostureZeroAndForeignTurnShape(t *testing.T) {
	t.Parallel()
	var p PermissionPosture
	if p != PostureDefault {
		t.Fatalf("zero PermissionPosture = %v, want PostureDefault", p)
	}
	tr := ForeignTurn{StartNew: true, ForeignSID: "sid"}
	if !tr.StartNew || tr.ForeignSID != "sid" {
		t.Fatal("ForeignTurn fields not wired")
	}
}
