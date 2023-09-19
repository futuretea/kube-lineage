package printers

import (
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	policyv1 "k8s.io/api/policy/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	unstructuredv1 "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/duration"
	"k8s.io/client-go/util/jsonpath"
	apiregistrationv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"

	"github.com/tohjustin/kube-lineage/internal/graph"
)

const (
	cellUnknown       = "<unknown>"
	cellNotApplicable = "-"
)

var (
	// objectColumnDefinitions holds table column definition for Kubernetes objects.
	objectColumnDefinitions = []metav1.TableColumnDefinition{
		{Name: "Name", Type: "string", Format: "name", Description: metav1.ObjectMeta{}.SwaggerDoc()["name"]},
		{Name: "Ready", Type: "string", Description: "The readiness state of this object."},
		{Name: "Status", Type: "string", Description: "The status of this object."},
		{Name: "Age", Type: "string", Description: metav1.ObjectMeta{}.SwaggerDoc()["creationTimestamp"]},
		{Name: "Relationships", Type: "array", Description: "The relationships this object has with its parent.", Priority: -1},
	}
	// objectReadyReasonJSONPath is the JSON path to get a Kubernetes object's
	// "Ready" condition reason.
	objectReadyReasonJSONPath = newJSONPath("reason", "{.status.conditions[?(@.type==\"Ready\")].reason}")
	// objectReadyStatusJSONPath is the JSON path to get a Kubernetes object's
	// "Ready" condition status.
	objectReadyStatusJSONPath = newJSONPath("status", "{.status.conditions[?(@.type==\"Ready\")].status}")
)

// createShowGroupFn creates a function that takes in a resource's kind &
// determines whether the resource's group should be included in its name.
func createShowGroupFn(nodeMap graph.NodeMap, showGroup bool, maxDepth uint) func(string) bool {
	// Create function that returns true, if showGroup is true
	if showGroup {
		return func(_ string) bool {
			return true
		}
	}

	// Track every object kind in the node map & the groups that they belong to.
	kindToGroupSetMap := map[string](map[string]struct{}){}
	for _, node := range nodeMap {
		if maxDepth != 0 && node.Depth > maxDepth {
			continue
		}
		if _, ok := kindToGroupSetMap[node.Kind]; !ok {
			kindToGroupSetMap[node.Kind] = map[string]struct{}{}
		}
		kindToGroupSetMap[node.Kind][node.Group] = struct{}{}
	}
	// When printing an object & if there exists another object in the node map
	// that has the same kind but belongs to a different group (eg. "services.v1"
	// vs "services.v1.serving.knative.dev"), we prepend the object's name with
	// its GroupKind instead of its Kind to clearly indicate which resource type
	// it belongs to.
	return func(kind string) bool {
		return len(kindToGroupSetMap[kind]) > 1
	}
}

// createShowNamespaceFn creates a function that takes in a resource's GroupKind
// & determines whether the resource's namespace should be shown.
func createShowNamespaceFn(nodeMap graph.NodeMap, showNamespace bool, maxDepth uint) func(schema.GroupKind) bool {
	showNS := showNamespace || shouldShowNamespace(nodeMap, maxDepth)
	if !showNS {
		return func(_ schema.GroupKind) bool {
			return false
		}
	}

	clusterScopeGKSet := map[schema.GroupKind]struct{}{}
	for _, node := range nodeMap {
		if maxDepth != 0 && node.Depth > maxDepth {
			continue
		}
		gk := node.GroupVersionKind().GroupKind()
		if !node.Namespaced {
			clusterScopeGKSet[gk] = struct{}{}
		}
	}
	return func(gk schema.GroupKind) bool {
		_, isClusterScopeGK := clusterScopeGKSet[gk]
		return !isClusterScopeGK
	}
}

// shouldShowNamespace determines whether namespace column should be shown.
// Returns true if objects in the provided node map are in different namespaces.
func shouldShowNamespace(nodeMap graph.NodeMap, maxDepth uint) bool {
	nsSet := map[string]struct{}{}
	for _, node := range nodeMap {
		if maxDepth != 0 && node.Depth > maxDepth {
			continue
		}
		ns := node.Namespace
		if _, ok := nsSet[ns]; !ok {
			nsSet[ns] = struct{}{}
		}
	}
	return len(nsSet) > 1
}

// newJSONPath returns a JSONPath object created from parsing the provided JSON
// path expression.
func newJSONPath(name, jsonPath string) *jsonpath.JSONPath {
	jp := jsonpath.New(name).AllowMissingKeys(true)
	if err := jp.Parse(jsonPath); err != nil {
		panic(err)
	}
	return jp
}

// getNestedString returns the field value of a Kubernetes object at the
// provided JSON path.
func getNestedString(data map[string]interface{}, jp *jsonpath.JSONPath) (string, error) {
	values, err := jp.FindResults(data)
	if err != nil {
		return "", err
	}
	strValues := []string{}
	for arrIx := range values {
		for valIx := range values[arrIx] {
			strValues = append(strValues, fmt.Sprintf("%v", values[arrIx][valIx].Interface()))
		}
	}
	str := strings.Join(strValues, ",")

	return str, nil
}

// getObjectReadyStatus returns the ready & status value of a Kubernetes object.
func getObjectReadyStatus(u *unstructuredv1.Unstructured) (string, string, error) {
	data := u.UnstructuredContent()
	ready, err := getNestedString(data, objectReadyStatusJSONPath)
	if err != nil {
		return "", "", err
	}
	status, err := getNestedString(data, objectReadyReasonJSONPath)
	if err != nil {
		return ready, "", err
	}

	return ready, status, nil
}

// getAPIServiceReadyStatus returns the ready & status value of a APIService
// which is based off the table cell values computed by printAPIService from
// https://github.com/kubernetes/kubernetes/blob/v1.22.1/pkg/printers/internalversion/printers.go.
func getAPIServiceReadyStatus(u *unstructuredv1.Unstructured) (string, string, error) {
	var apisvc apiregistrationv1.APIService
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.UnstructuredContent(), &apisvc)
	if err != nil {
		return "", "", err
	}
	var ready, status string
	for _, condition := range apisvc.Status.Conditions {
		if condition.Type == apiregistrationv1.Available {
			ready = string(condition.Status)
			if condition.Status != apiregistrationv1.ConditionTrue {
				status = condition.Reason
			}
		}
	}

	return ready, status, nil
}

// getDaemonSetReadyStatus returns the ready & status value of a DaemonSet
// which is based off the table cell values computed by printDaemonSet from
// https://github.com/kubernetes/kubernetes/blob/v1.22.1/pkg/printers/internalversion/printers.go.
//
//nolint:unparam
func getDaemonSetReadyStatus(u *unstructuredv1.Unstructured) (string, string, error) {
	var ds appsv1.DaemonSet
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.UnstructuredContent(), &ds)
	if err != nil {
		return "", "", err
	}
	desiredReplicas := ds.Status.DesiredNumberScheduled
	readyReplicas := ds.Status.NumberReady
	ready := fmt.Sprintf("%d/%d", readyReplicas, desiredReplicas)

	return ready, "", nil
}

// getDeploymentReadyStatus returns the ready & status value of a Deployment
// which is based off the table cell values computed by printDeployment from
// https://github.com/kubernetes/kubernetes/blob/v1.22.1/pkg/printers/internalversion/printers.go.
//
//nolint:unparam
func getDeploymentReadyStatus(u *unstructuredv1.Unstructured) (string, string, error) {
	var deploy appsv1.Deployment
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.UnstructuredContent(), &deploy)
	if err != nil {
		return "", "", err
	}
	desiredReplicas := deploy.Status.Replicas
	readyReplicas := deploy.Status.ReadyReplicas
	ready := fmt.Sprintf("%d/%d", readyReplicas, desiredReplicas)

	return ready, "", nil
}

// getEventCoreReadyStatus returns the ready & status value of a Event.
//
//nolint:unparam
func getEventCoreReadyStatus(u *unstructuredv1.Unstructured) (string, string, error) {
	var status string
	var ev corev1.Event
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.UnstructuredContent(), &ev)
	if err != nil {
		return "", "", err
	}
	if ev.Count > 1 {
		status = fmt.Sprintf("%s: %s (x%d)", ev.Reason, ev.Message, ev.Count)
	} else {
		status = fmt.Sprintf("%s: %s", ev.Reason, ev.Message)
	}

	return "", status, nil
}

// getEventReadyStatus returns the ready & status value of a Event.events.k8s.io.
//
//nolint:unparam
func getEventReadyStatus(u *unstructuredv1.Unstructured) (string, string, error) {
	var status string
	var ev eventsv1.Event
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.UnstructuredContent(), &ev)
	if err != nil {
		return "", "", err
	}
	if ev.DeprecatedCount > 1 {
		status = fmt.Sprintf("%s: %s (x%d)", ev.Reason, ev.Note, ev.DeprecatedCount)
	} else {
		status = fmt.Sprintf("%s: %s", ev.Reason, ev.Note)
	}

	return "", status, nil
}

// getPodReadyStatus returns the ready & status value of a Pod which is based
// off the table cell values computed by printPod from
// https://github.com/kubernetes/kubernetes/blob/v1.22.1/pkg/printers/internalversion/printers.go.
//
//nolint:funlen,gocognit,gocyclo
func getPodReadyStatus(u *unstructuredv1.Unstructured) (string, string, error) {
	var pod corev1.Pod
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.UnstructuredContent(), &pod)
	if err != nil {
		return "", "", err
	}
	totalContainers := len(pod.Spec.Containers)
	readyContainers := 0
	reason := string(pod.Status.Phase)
	if len(pod.Status.Reason) > 0 {
		reason = pod.Status.Reason
	}
	initializing := false
	for i := range pod.Status.InitContainerStatuses {
		container := pod.Status.InitContainerStatuses[i]
		state := container.State
		switch {
		case state.Terminated != nil && state.Terminated.ExitCode == 0:
			continue
		case state.Terminated != nil && len(state.Terminated.Reason) > 0:
			reason = state.Terminated.Reason
		case state.Terminated != nil && len(state.Terminated.Reason) == 0 && state.Terminated.Signal != 0:
			reason = fmt.Sprintf("Signal:%d", state.Terminated.Signal)
		case state.Terminated != nil && len(state.Terminated.Reason) == 0 && state.Terminated.Signal == 0:
			reason = fmt.Sprintf("ExitCode:%d", state.Terminated.ExitCode)
		case state.Waiting != nil && len(state.Waiting.Reason) > 0 && state.Waiting.Reason != "PodInitializing":
			reason = state.Waiting.Reason
		default:
			reason = fmt.Sprintf("%d/%d", i, len(pod.Spec.InitContainers))
		}
		reason = fmt.Sprintf("Init:%s", reason)
		initializing = true
		break
	}
	if !initializing {
		hasRunning := false
		for i := len(pod.Status.ContainerStatuses) - 1; i >= 0; i-- {
			container := pod.Status.ContainerStatuses[i]
			state := container.State
			switch {
			case state.Terminated != nil && len(state.Terminated.Reason) > 0:
				reason = state.Terminated.Reason
			case state.Terminated != nil && len(state.Terminated.Reason) == 0 && state.Terminated.Signal != 0:
				reason = fmt.Sprintf("Signal:%d", state.Terminated.Signal)
			case state.Terminated != nil && len(state.Terminated.Reason) == 0 && state.Terminated.Signal == 0:
				reason = fmt.Sprintf("ExitCode:%d", state.Terminated.ExitCode)
			case state.Waiting != nil && len(state.Waiting.Reason) > 0:
				reason = state.Waiting.Reason
			case state.Running != nil && container.Ready:
				hasRunning = true
				readyContainers++
			}
		}
		// change pod status back to "Running" if there is at least one container still reporting as "Running" status
		if reason == "Completed" && hasRunning {
			reason = "NotReady"
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
					reason = "Running"
				}
			}
		}
	}
	if pod.DeletionTimestamp != nil {
		// Hardcode "k8s.io/kubernetes/pkg/util/node.NodeUnreachablePodReason" as
		// "NodeLost" so we don't need import the entire k8s.io/kubernetes package
		if pod.Status.Reason == "NodeLost" {
			reason = "Unknown"
		} else {
			reason = "Terminating"
		}
	}
	ready := fmt.Sprintf("%d/%d", readyContainers, totalContainers)

	return ready, reason, nil
}

// getPodDisruptionBudgetReadyStatus returns the ready & status value of a
// PodDisruptionBudget.
//
//nolint:unparam
func getPodDisruptionBudgetReadyStatus(u *unstructuredv1.Unstructured) (string, string, error) {
	var pdb policyv1.PodDisruptionBudget
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.UnstructuredContent(), &pdb)
	if err != nil {
		return "", "", err
	}
	var status string
	for _, condition := range pdb.Status.Conditions {
		if condition.ObservedGeneration == pdb.Generation {
			if condition.Type == policyv1.DisruptionAllowedCondition {
				status = condition.Reason
			}
		}
	}

	return "", status, nil
}

// getReplicaSetReadyStatus returns the ready & status value of a ReplicaSet
// which is based off the table cell values computed by printReplicaSet from
// https://github.com/kubernetes/kubernetes/blob/v1.22.1/pkg/printers/internalversion/printers.go.
//
//nolint:unparam
func getReplicaSetReadyStatus(u *unstructuredv1.Unstructured) (string, string, error) {
	var rs appsv1.ReplicaSet
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.UnstructuredContent(), &rs)
	if err != nil {
		return "", "", err
	}
	desiredReplicas := rs.Status.Replicas
	readyReplicas := rs.Status.ReadyReplicas
	ready := fmt.Sprintf("%d/%d", readyReplicas, desiredReplicas)

	return ready, "", nil
}

// getReplicationControllerReadyStatus returns the ready & status value of a
// ReplicationController which is based off the table cell values computed by
// printReplicationController from
// https://github.com/kubernetes/kubernetes/blob/v1.22.1/pkg/printers/internalversion/printers.go.
//
//nolint:unparam
func getReplicationControllerReadyStatus(u *unstructuredv1.Unstructured) (string, string, error) {
	var rc corev1.ReplicationController
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.UnstructuredContent(), &rc)
	if err != nil {
		return "", "", err
	}
	desiredReplicas := rc.Status.Replicas
	readyReplicas := rc.Status.ReadyReplicas
	ready := fmt.Sprintf("%d/%d", readyReplicas, desiredReplicas)

	return ready, "", nil
}

// getStatefulSetReadyStatus returns the ready & status value of a StatefulSet
// which is based off the table cell values computed by printStatefulSet from
// https://github.com/kubernetes/kubernetes/blob/v1.22.1/pkg/printers/internalversion/printers.go.
//
//nolint:unparam
func getStatefulSetReadyStatus(u *unstructuredv1.Unstructured) (string, string, error) {
	var sts appsv1.StatefulSet
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.UnstructuredContent(), &sts)
	if err != nil {
		return "", "", err
	}
	desiredReplicas := sts.Status.Replicas
	readyReplicas := sts.Status.ReadyReplicas
	ready := fmt.Sprintf("%d/%d", readyReplicas, desiredReplicas)

	return ready, "", nil
}

// getVolumeAttachmentReadyStatus returns the ready & status value of a
// VolumeAttachment.
func getVolumeAttachmentReadyStatus(u *unstructuredv1.Unstructured) (string, string, error) {
	var va storagev1.VolumeAttachment
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.UnstructuredContent(), &va)
	if err != nil {
		return "", "", err
	}
	var ready, status string
	if va.Status.Attached {
		ready = "True"
	} else {
		ready = "False"
	}
	var errTime time.Time
	if err := va.Status.AttachError; err != nil {
		status = err.Message
		errTime = err.Time.Time
	}
	if err := va.Status.DetachError; err != nil {
		if err.Time.After(errTime) {
			status = err.Message
		}
	}

	return ready, status, nil
}

// nodeToTableRow converts the provided node into a table row.
//
//nolint:funlen,gocognit,goconst
func nodeToTableRow(node *graph.Node, rset graph.RelationshipSet, namePrefix string, showGroupFn func(kind string) bool) metav1.TableRow {
	var name, ready, status, age string
	var relationships interface{}

	switch {
	case len(node.Kind) == 0:
		name = node.Name
	case len(node.Group) > 0 && showGroupFn(node.Kind):
		name = fmt.Sprintf("%s%s.%s/%s", namePrefix, node.Kind, node.Group, node.Name)
	default:
		name = fmt.Sprintf("%s%s/%s", namePrefix, node.Kind, node.Name)
	}
	switch {
	case node.Group == corev1.GroupName && node.Kind == "Event":
		ready, status, _ = getEventCoreReadyStatus(node.Unstructured)
	case node.Group == corev1.GroupName && node.Kind == "Pod":
		ready, status, _ = getPodReadyStatus(node.Unstructured)
	case node.Group == corev1.GroupName && node.Kind == "ReplicationController":
		ready, status, _ = getReplicationControllerReadyStatus(node.Unstructured)
	case node.Group == appsv1.GroupName && node.Kind == "DaemonSet":
		ready, status, _ = getDaemonSetReadyStatus(node.Unstructured)
	case node.Group == appsv1.GroupName && node.Kind == "Deployment":
		ready, status, _ = getDeploymentReadyStatus(node.Unstructured)
	case node.Group == appsv1.GroupName && node.Kind == "ReplicaSet":
		ready, status, _ = getReplicaSetReadyStatus(node.Unstructured)
	case node.Group == appsv1.GroupName && node.Kind == "StatefulSet":
		ready, status, _ = getStatefulSetReadyStatus(node.Unstructured)
	case node.Group == policyv1.GroupName && node.Kind == "PodDisruptionBudget":
		ready, status, _ = getPodDisruptionBudgetReadyStatus(node.Unstructured)
	case node.Group == apiregistrationv1.GroupName && node.Kind == "APIService":
		ready, status, _ = getAPIServiceReadyStatus(node.Unstructured)
	case node.Group == eventsv1.GroupName && node.Kind == "Event":
		ready, status, _ = getEventReadyStatus(node.Unstructured)
	case node.Group == storagev1.GroupName && node.Kind == "VolumeAttachment":
		ready, status, _ = getVolumeAttachmentReadyStatus(node.Unstructured)
	case node.Unstructured != nil:
		ready, status, _ = getObjectReadyStatus(node.Unstructured)
	}
	if len(ready) == 0 {
		ready = cellNotApplicable
	}
	if node.Unstructured != nil {
		age = translateTimestampSince(node.GetCreationTimestamp())
	}
	relationships = []string{}
	if rset != nil {
		relationships = rset.List()
	}

	return metav1.TableRow{
		Object: runtime.RawExtension{Object: node.DeepCopyObject()},
		Cells: []interface{}{
			name,
			ready,
			status,
			age,
			relationships,
		},
	}
}

// nodeMapToTable converts the provided node & either its dependencies or
// dependents into table rows.
func nodeMapToTable(
	nodeMap graph.NodeMap,
	root *graph.Node,
	maxDepth uint,
	depsIsDependencies bool,
	showGroupFn func(kind string) bool) (*metav1.Table, error) {
	// Sorts the list of UIDs based on the underlying object in following order:
	// Namespace, Kind, Group, Name
	sortDepsFn := func(d map[types.UID]graph.RelationshipSet) []types.UID {
		nodes, ix := make(graph.NodeList, len(d)), 0
		for uid := range d {
			nodes[ix] = nodeMap[uid]
			ix++
		}
		sort.Sort(nodes)
		sortedUIDs := make([]types.UID, len(d))
		for ix, node := range nodes {
			sortedUIDs[ix] = node.UID
		}
		return sortedUIDs
	}

	var rows []metav1.TableRow
	row := nodeToTableRow(root, nil, "", showGroupFn)
	uidSet := map[types.UID]struct{}{}
	depRows, err := nodeDepsToTableRows(nodeMap, uidSet, root, "", 1, maxDepth, depsIsDependencies, sortDepsFn, showGroupFn)
	if err != nil {
		return nil, err
	}
	rows = append(rows, row)
	rows = append(rows, depRows...)
	table := metav1.Table{
		ColumnDefinitions: objectColumnDefinitions,
		Rows:              rows,
	}

	return &table, nil
}

// nodeDepsToTableRows converts either the dependencies or dependents of the
// provided node into table rows.
func nodeDepsToTableRows(
	nodeMap graph.NodeMap,
	uidSet map[types.UID]struct{},
	node *graph.Node,
	prefix string,
	depth uint,
	maxDepth uint,
	depsIsDependencies bool,
	sortDepsFn func(d map[types.UID]graph.RelationshipSet) []types.UID,
	showGroupFn func(kind string) bool) ([]metav1.TableRow, error) {
	rows := make([]metav1.TableRow, 0, len(nodeMap))

	// Guard against possible cycles
	if _, ok := uidSet[node.UID]; ok {
		return rows, nil
	}
	uidSet[node.UID] = struct{}{}

	deps := node.GetDeps(depsIsDependencies)
	depUIDs := sortDepsFn(deps)
	lastIx := len(depUIDs) - 1
	for ix, childUID := range depUIDs {
		var childPrefix, depPrefix string
		if ix != lastIx {
			childPrefix, depPrefix = prefix+"├── ", prefix+"│   "
		} else {
			childPrefix, depPrefix = prefix+"└── ", prefix+"    "
		}

		child, ok := nodeMap[childUID]
		if !ok {
			return nil, fmt.Errorf("dependent object (uid: %s) not found in list of fetched objects", childUID)
		}
		rset, ok := deps[childUID]
		if !ok {
			return nil, fmt.Errorf("dependent object (uid: %s) not found", childUID)
		}
		row := nodeToTableRow(child, rset, childPrefix, showGroupFn)
		rows = append(rows, row)
		if maxDepth == 0 || depth < maxDepth {
			depRows, err := nodeDepsToTableRows(nodeMap, uidSet, child, depPrefix, depth+1, maxDepth, depsIsDependencies, sortDepsFn, showGroupFn)
			if err != nil {
				return nil, err
			}
			rows = append(rows, depRows...)
		}
	}

	return rows, nil
}

// translateTimestampSince returns the elapsed time since timestamp in
// human-readable approximation.
func translateTimestampSince(timestamp metav1.Time) string {
	if timestamp.IsZero() {
		return cellUnknown
	}

	return duration.HumanDuration(time.Since(timestamp.Time))
}
