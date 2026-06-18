package changefeed_test

import (
	"testing"

	"github.com/kennguy3n/zk-drive/internal/changefeed"
	"github.com/kennguy3n/zk-drive/internal/permission"
)

// resourceTypeToKind maps every value of permission.ResourceType
// to the changefeed.Kind* constant that represents structural
// mutations of that resource type for cache-invalidation purposes.
//
// The map is the structural coupling this test enforces: the
// permission cache's correctness depends on the
// changefeed bust matrix firing for every (Kind, op) pair where
// the op could reshape the inheritance tree of a resource of that
// type. Until this test landed, the coupling was implicit — a
// future ResourceDocument added to internal/permission/models.go
// would silently leave document moves un-busted because
// shouldBustForMutation already returned false for KindDocument.
//
// CONTRACT for adding a new permission.Resource* constant:
//  1. Add the constant in internal/permission/models.go and
//     append it to AllResourceTypes there (enforced by
//     TestAllResourceTypesIsComplete).
//  2. Add the (resource_type → changefeed_kind) entry to this
//     map. The test below trips when AllResourceTypes contains
//     a resource type missing from this map.
//  3. Set the corresponding kind's move (and delete, if the
//     resource can have descendants) decision to bust=true in
//     knownKindOpBustDecisions (service_test.go).
//
// The test enforces step 2 by iterating
// permission.AllResourceTypes; step 3 is then enforced by
// reading knownKindOpBustDecisions.
var resourceTypeToKind = map[string]string{
	permission.ResourceFile:   changefeed.KindFile,
	permission.ResourceFolder: changefeed.KindFolder,
}

// TestPermissionResourceTypesCoupleToChangefeedKinds enforces
// the structural invariant that every value of
// permission.ResourceType has a corresponding changefeed.Kind
// whose 'move' op (at minimum) busts the cache. Folders
// additionally require 'delete' to bust because deleting a
// folder reshapes ancestry for descendant resources.
//
// This is the cross-package coupling the Kind-only audit cannot
// cover: TestShouldBustForMutation_ExhaustivelyAuditsKindOpMatrix
// in service_test.go catches new Kind constants but not changes to
// the permission resource-type vocabulary that would silently
// repurpose an existing Kind for cache-affecting mutations.
//
// The test imports BOTH packages (only safe in _test.go because
// neither package imports the other in production), reads the
// canonical permission.AllResourceTypes registry, and asserts
// each entry has a Kind mapping that survives the bust audit.
func TestPermissionResourceTypesCoupleToChangefeedKinds(t *testing.T) {
	t.Parallel()

	// Step 1: every value in permission.AllResourceTypes
	// must have an entry in resourceTypeToKind. Adding a new
	// ResourceType in models.go without updating this map
	// would otherwise create a silent stale-cache risk.
	for _, rt := range permission.AllResourceTypes {
		if _, ok := resourceTypeToKind[rt]; !ok {
			t.Errorf("permission.AllResourceTypes contains %q but resourceTypeToKind has no mapping — every resource type MUST map to a changefeed Kind so the bust matrix can be audited against it. See doc comment on resourceTypeToKind for the 3-step add-a-resource-type workflow", rt)
		}
	}
	if got := len(resourceTypeToKind); got != len(permission.AllResourceTypes) {
		t.Errorf("resourceTypeToKind has %d entries but permission.AllResourceTypes has %d — registry drift between packages", got, len(permission.AllResourceTypes))
	}

	// Step 2: every mapped (resource_type → kind) must have
	// its 'move' op set to bust=true in
	// knownKindOpBustDecisions. A resource moving between
	// parents with different grants is the canonical
	// cache-affecting event we must invalidate on.
	for resType, kind := range resourceTypeToKind {
		moveKey := kind + "/" + changefeed.OpMove
		decision, ok := knownKindOpBustDecisions[moveKey]
		if !ok {
			t.Errorf("resource type %q maps to kind %q but knownKindOpBustDecisions missing %q — the Kind-level audit ledger must cover every (kind, op) pair", resType, kind, moveKey)
			continue
		}
		if !decision {
			t.Errorf("resource type %q maps to kind %q whose 'move' op does NOT bust cache (knownKindOpBustDecisions[%q]=false) — a resource moving between parents with different grants would leave stale cached access checks for up to TTL. Either flip the bust decision to true in service_test.go AND update shouldBustForMutation in service.go, OR document why moves of this resource type cannot affect cached access checks", resType, kind, moveKey)
		}
	}

	// Step 3: special case — KindFolder is the only kind
	// whose 'delete' must also bust because deleting a folder
	// shifts the ancestor chain for every descendant. File
	// delete is bounded-stale (see service.go doc comment).
	// We assert KindFolder's delete decision explicitly so a
	// future refactor that flips it to false fails here.
	if !knownKindOpBustDecisions[changefeed.KindFolder+"/"+changefeed.OpDelete] {
		t.Errorf("KindFolder/delete must bust cache (folder deletion reshapes descendant ancestry); knownKindOpBustDecisions disagrees")
	}
}
