package decision

import (
	"testing"
	"time"
)

func TestFromWireRefusesAVerdictThatDisagreesWithItsReason(t *testing.T) {
	for name, w := range map[string]Wire{
		"allow with a deny reason":                {Allowed: true, Reason: GrantAbsent},
		"deny with an allow reason":               {Allowed: false, Reason: AllowedByRole},
		"deny with the accept-ahead allow reason": {Allowed: false, Reason: AllowedByPlatformAdmin},
		"allow with a local reason":               {Allowed: true, Reason: Timeout},
		"allow with no reason":                    {Allowed: true},
	} {
		d := FromWire(w)
		if d.Allowed() || !d.Local() || d.Reason() != Malformed || d.Err() == nil {
			t.Errorf("%s: allowed=%v reason=%s", name, d.Allowed(), d.Reason())
		}
	}
	d := FromWire(Wire{Allowed: true, Reason: AllowedByRole, ExpiresAt: time.Unix(1, 0), Obligations: NewObligations(ObligationSpec{})})
	if !d.Allowed() || d.Local() {
		t.Fatal("a consistent allow was refused")
	}
	d = FromWire(Wire{Allowed: true, Reason: AllowedByPlatformAdmin, ExpiresAt: time.Unix(1, 0), Obligations: NewObligations(ObligationSpec{})})
	if !d.Allowed() || d.Local() || d.Reason() != AllowedByPlatformAdmin {
		t.Fatal("a consistent allow with the accept-ahead reason was refused")
	}
}

func TestADenyDropsObligations(t *testing.T) {
	d := FromWire(Wire{Reason: GrantAbsent, Obligations: NewObligations(ObligationSpec{Tenant: "org_x"})})
	if d.Allowed() || d.Obligations().TenantAllows("org_x") {
		t.Fatal("a deny carried obligations")
	}
}
