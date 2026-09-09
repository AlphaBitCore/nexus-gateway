package hubapi

import (
	"reflect"
	"strings"
	"sync"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/observability/consumer"
)

// The /things/audit HTTP fallback forwards a DEVICE-SUPPLIED event map to NATS.
// Before this file it forwarded it verbatim, stamping thingId/thingName/source
// on top — which let a device self-assert server-owned attribution
// (entityId, orgId, identity, …) and even override the stamp.
//
// AN EXACT-STRING DENYLIST WOULD NOT HAVE WORKED, and that is the whole reason
// this is written the way it is: the downstream consumer decodes with
// goccy/go-json, whose struct-field matching is CASE-INSENSITIVE. A forged
// "EntityId" or "ORGID" binds anyway, and a lowercase "thingid" sorts after the
// server-stamped canonical "thingId" when the map is marshalled, so it wins on
// last-key-wins. Every comparison here is therefore made on the folded key.
//
// The permitted set is DERIVED, not hand-listed: it is every wire key the
// consumer can decode, minus the server-owned keys named below. Deriving it
// means a new consumer field cannot silently start being dropped (which would
// lose agent data), while auditKeysServerOwned means a new ATTRIBUTION field
// cannot silently start being forgeable — that one has to be added here, and
// TestAuditAllowlist_ServerOwnedKeysAreRealFields fails if it is misspelled.

// auditKeysServerOwned are the keys the HUB decides and a device may never
// assert. Attribution answers "whose traffic is this" and is resolved from the
// mTLS-bound identity; the producer-trust keys answer "how much may we believe
// about this row" and a device claiming them is claiming its own trustworthiness.
var auditKeysServerOwned = []string{
	// Attribution — resolved Hub-side from the authenticated Thing.
	"thingId", "thingName",
	"entityType", "entityId", "entityName",
	"orgId", "orgName", "identity",
	// Producer trust — a device asserting these is grading its own homework.
	"internalPurpose", "attestationVerified", "attestationAgentId",
	"apiKeyClass", "apiKeyFingerprint", "credentialId",
}

// auditKeysDeviceMayNotAssert are server-owned for a DEVICE-token caller and
// legitimate for a SERVICE-token one.
//
// `source` is the only member, and the split is the existing trust model in this
// file rather than a new one: requireMutationAuthority already treats a device
// token as bound to its own identity and a fleet-shared service token as
// trusted. The handler's own contract says a Hub-internal caller "should pre-set
// evt[\"source\"]" when it needs a different label, so stripping it from every
// caller would break that path — while leaving it settable by a DEVICE would let
// an agent file its traffic under another producer's label.
var auditKeysDeviceMayNotAssert = []string{"source"}

var (
	auditAllowOnce sync.Once
	// auditAllowDevice excludes both server-owned sets; auditAllowService
	// excludes only the always-server-owned one.
	auditAllowDevice  map[string]struct{}
	auditAllowService map[string]struct{}
)

func auditKeySets() (device, service map[string]struct{}) {
	auditAllowOnce.Do(func() {
		alwaysDenied := make(map[string]struct{}, len(auditKeysServerOwned))
		for _, k := range auditKeysServerOwned {
			alwaysDenied[strings.ToLower(k)] = struct{}{}
		}
		deviceDenied := make(map[string]struct{}, len(auditKeysDeviceMayNotAssert))
		for _, k := range auditKeysDeviceMayNotAssert {
			deviceDenied[strings.ToLower(k)] = struct{}{}
		}
		auditAllowDevice = make(map[string]struct{}, 128)
		auditAllowService = make(map[string]struct{}, 128)
		for _, tag := range consumerWireKeys() {
			lower := strings.ToLower(tag)
			if _, denied := alwaysDenied[lower]; denied {
				continue
			}
			auditAllowService[lower] = struct{}{}
			if _, denied := deviceDenied[lower]; denied {
				continue
			}
			auditAllowDevice[lower] = struct{}{}
		}
	})
	return auditAllowDevice, auditAllowService
}

// consumerWireKeys returns every json tag on the message the traffic consumer
// decodes. Reflection rather than a copied list: a copied list is a second
// definition of the wire contract, and the two would drift.
func consumerWireKeys() []string {
	t := reflect.TypeOf(consumer.TrafficEventMessage{})
	out := make([]string, 0, t.NumField())
	for i := range t.NumField() {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		if comma := strings.IndexByte(tag, ','); comma >= 0 {
			tag = tag[:comma]
		}
		if tag != "" {
			out = append(out, tag)
		}
	}
	return out
}

// stripNonAgentAuditKeys deletes every key the caller may not assert, in any
// capitalisation, and reports how many it removed so the caller can log a
// forgery attempt rather than swallow it.
//
// deviceCaller selects the stricter set. It runs BEFORE the Hub re-stamps the
// authoritative values, so a stripped forgery cannot race the stamp.
func stripNonAgentAuditKeys(evt map[string]any, deviceCaller bool) int {
	deviceAllow, serviceAllow := auditKeySets()
	allow := serviceAllow
	if deviceCaller {
		allow = deviceAllow
	}
	var stripped int
	for k := range evt {
		if _, ok := allow[strings.ToLower(k)]; !ok {
			delete(evt, k)
			stripped++
		}
	}
	return stripped
}
