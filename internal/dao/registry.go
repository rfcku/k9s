// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of K9s

package dao

import (
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/derailed/k9s/internal/client"
	"github.com/derailed/k9s/internal/slogs"
	apiext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
)

const (
	crdCat   = "crd"
	k9sCat   = "k9s"
	helmCat  = "helm"
	scaleCat = "scale"
	crdGVR   = "apiextensions.k8s.io/v1/customresourcedefinitions"
)

// MetaAccess tracks resources metadata.
var MetaAccess = NewMeta()

var stdGroups = sets.New[string](
	"apps/v1",
	"autoscaling/v1",
	"autoscaling/v2",
	"autoscaling/v2beta1",
	"autoscaling/v2beta2",
	"batch/v1",
	"batch/v1beta1",
	"extensions/v1beta1",
	"policy/v1beta1",
	"policy/v1",
	"v1",
)

func (m ResourceMetas) clear() {
	for k := range m {
		delete(m, k)
	}
}

// Meta represents available resource metas.
type Meta struct {
	resMetas ResourceMetas
	mx       sync.RWMutex
}

// NewMeta returns a resource meta.
func NewMeta() *Meta {
	return &Meta{resMetas: make(ResourceMetas)}
}

// AccessorFor returns a client accessor for a resource if registered.
// Otherwise it returns a generic accessor.
// Customize here for non resource types or types with metrics or logs.
func AccessorFor(f Factory, gvr client.GVR) (Accessor, error) {
	m := Accessors{
		client.NewGVR("workloads"):                                         &Workload{},
		client.NewGVR("contexts"):                                          &Context{},
		client.NewGVR("containers"):                                        &Container{},
		client.NewGVR("scans"):                                             &ImageScan{},
		client.NewGVR("screendumps"):                                       &ScreenDump{},
		client.NewGVR("benchmarks"):                                        &Benchmark{},
		client.NewGVR("portforwards"):                                      &PortForward{},
		client.NewGVR("dir"):                                               &Dir{},
		client.NewGVR("v1/services"):                                       &Service{},
		client.NewGVR("v1/pods"):                                           &Pod{},
		client.NewGVR("v1/nodes"):                                          &Node{},
		client.NewGVR("v1/namespaces"):                                     &Namespace{},
		client.NewGVR("v1/configmaps"):                                     &ConfigMap{},
		client.NewGVR("v1/secrets"):                                        &Secret{},
		client.NewGVR("apps/v1/deployments"):                               &Deployment{},
		client.NewGVR("apps/v1/daemonsets"):                                &DaemonSet{},
		client.NewGVR("apps/v1/statefulsets"):                              &StatefulSet{},
		client.NewGVR("apps/v1/replicasets"):                               &ReplicaSet{},
		client.NewGVR("batch/v1/cronjobs"):                                 &CronJob{},
		client.NewGVR("batch/v1beta1/cronjobs"):                            &CronJob{},
		client.NewGVR("batch/v1/jobs"):                                     &Job{},
		client.NewGVR("helm"):                                              &HelmChart{},
		client.NewGVR("helm-history"):                                      &HelmHistory{},
		client.NewGVR("apiextensions.k8s.io/v1/customresourcedefinitions"): &CustomResourceDefinition{},
	}

	r, ok := m[gvr]
	if !ok {
		r = new(Scaler)
		slog.Debug("No DAO registry entry. Using generics!", slogs.GVR, gvr)
	}
	r.Init(f, gvr)

	return r, nil
}

// RegisterMeta registers a new resource meta object.
func (m *Meta) RegisterMeta(gvr string, res metav1.APIResource) {
	m.mx.Lock()
	defer m.mx.Unlock()

	m.resMetas[client.NewGVR(gvr)] = res
}

// AllGVRs returns all cluster resources.
func (m *Meta) AllGVRs() client.GVRs {
	m.mx.RLock()
	defer m.mx.RUnlock()

	kk := make(client.GVRs, 0, len(m.resMetas))
	for k := range m.resMetas {
		kk = append(kk, k)
	}
	sort.Sort(kk)

	return kk
}

// GVK2GVR convert gvk to gvr
func (m *Meta) GVK2GVR(gv schema.GroupVersion, kind string) (client.GVR, bool, bool) {
	m.mx.RLock()
	defer m.mx.RUnlock()

	for gvr, meta := range m.resMetas {
		if gv.Group == meta.Group && gv.Version == meta.Version && kind == meta.Kind {
			return gvr, meta.Namespaced, true
		}
	}

	return client.NoGVR, false, false
}

// MetaFor returns a resource metadata for a given gvr.
func (m *Meta) MetaFor(gvr client.GVR) (metav1.APIResource, error) {
	m.mx.RLock()
	defer m.mx.RUnlock()

	meta, ok := m.resMetas[gvr]
	if !ok {
		return metav1.APIResource{}, fmt.Errorf("no resource meta defined for %q", gvr)
	}
	return meta, nil
}

// IsCRD checks if resource represents a CRD
func IsCRD(r metav1.APIResource) bool {
	return slices.Contains(r.Categories, crdCat)
}

// IsK8sMeta checks for non resource meta.
func IsK8sMeta(m metav1.APIResource) bool {
	return !slices.ContainsFunc(m.Categories, func(category string) bool {
		return category == k9sCat || category == helmCat
	})
}

// IsK9sMeta checks for non resource meta.
func IsK9sMeta(m metav1.APIResource) bool {
	return slices.Contains(m.Categories, k9sCat)
}

// IsScalable check if the resource can be scaled
func IsScalable(m metav1.APIResource) bool {
	return slices.Contains(m.Categories, scaleCat)
}

// LoadResources hydrates server preferred+CRDs resource metadata.
func (m *Meta) LoadResources(f Factory) error {
	m.mx.Lock()
	defer m.mx.Unlock()

	m.resMetas.clear()
	if err := loadPreferred(f, m.resMetas); err != nil {
		return err
	}
	loadNonResource(m.resMetas)

	// We've actually loaded all the CRDs in loadPreferred, and we're now adding
	// some additional CRD properties on top of that.
	loadCRDs(f, m.resMetas)

	return nil
}

// BOZO!! Need countermeasures for direct commands!
func loadNonResource(m ResourceMetas) {
	loadK9s(m)
	loadRBAC(m)
	loadHelm(m)
}

func loadK9s(m ResourceMetas) {
	m[client.NewGVR("workloads")] = metav1.APIResource{
		Name:         "workloads",
		Kind:         "Workload",
		SingularName: "workload",
		Namespaced:   true,
		ShortNames:   []string{"wk"},
		Categories:   []string{k9sCat},
	}
	m[client.NewGVR("pulses")] = metav1.APIResource{
		Name:         "pulses",
		Kind:         "Pulse",
		SingularName: "pulses",
		ShortNames:   []string{"hz", "pu"},
		Categories:   []string{k9sCat},
	}
	m[client.NewGVR("dir")] = metav1.APIResource{
		Name:         "dir",
		Kind:         "Dir",
		SingularName: "dir",
		Categories:   []string{k9sCat},
	}
	m[client.NewGVR("xrays")] = metav1.APIResource{
		Name:         "xray",
		Kind:         "XRays",
		SingularName: "xray",
		Categories:   []string{k9sCat},
	}
	m[client.NewGVR("references")] = metav1.APIResource{
		Name:         "references",
		Kind:         "References",
		SingularName: "reference",
		Verbs:        []string{},
		Categories:   []string{k9sCat},
	}
	m[client.NewGVR("aliases")] = metav1.APIResource{
		Name:         "aliases",
		Kind:         "Aliases",
		SingularName: "alias",
		Verbs:        []string{},
		Categories:   []string{k9sCat},
	}
	m[client.NewGVR("contexts")] = metav1.APIResource{
		Name:         "contexts",
		Kind:         "Contexts",
		SingularName: "context",
		ShortNames:   []string{"ctx"},
		Verbs:        []string{},
		Categories:   []string{k9sCat},
	}
	m[client.NewGVR("screendumps")] = metav1.APIResource{
		Name:         "screendumps",
		Kind:         "ScreenDumps",
		SingularName: "screendump",
		ShortNames:   []string{"sd"},
		Verbs:        []string{"delete"},
		Categories:   []string{k9sCat},
	}
	m[client.NewGVR("benchmarks")] = metav1.APIResource{
		Name:         "benchmarks",
		Kind:         "Benchmarks",
		SingularName: "benchmark",
		ShortNames:   []string{"be"},
		Verbs:        []string{"delete"},
		Categories:   []string{k9sCat},
	}
	m[client.NewGVR("portforwards")] = metav1.APIResource{
		Name:         "portforwards",
		Namespaced:   true,
		Kind:         "PortForwards",
		SingularName: "portforward",
		ShortNames:   []string{"pf"},
		Verbs:        []string{"delete"},
		Categories:   []string{k9sCat},
	}
	m[client.NewGVR("containers")] = metav1.APIResource{
		Name:         "containers",
		Kind:         "Containers",
		SingularName: "container",
		Verbs:        []string{},
		Categories:   []string{k9sCat},
	}
	m[client.NewGVR("scans")] = metav1.APIResource{
		Name:         "scans",
		Kind:         "Scans",
		SingularName: "scan",
		Verbs:        []string{},
		Categories:   []string{k9sCat},
	}
}

func loadHelm(m ResourceMetas) {
	m[client.NewGVR("helm")] = metav1.APIResource{
		Name:       "helm",
		Kind:       "Helm",
		Namespaced: true,
		Verbs:      []string{"delete"},
		Categories: []string{helmCat},
	}
	m[client.NewGVR("helm-history")] = metav1.APIResource{
		Name:       "history",
		Kind:       "History",
		Namespaced: true,
		Verbs:      []string{"delete"},
		Categories: []string{helmCat},
	}
}

func loadRBAC(m ResourceMetas) {
	m[client.NewGVR("rbac")] = metav1.APIResource{
		Name:       "rbacs",
		Kind:       "Rules",
		Categories: []string{k9sCat},
	}
	m[client.NewGVR("policy")] = metav1.APIResource{
		Name:       "policies",
		Kind:       "Rules",
		Namespaced: true,
		Categories: []string{k9sCat},
	}
	m[client.NewGVR("users")] = metav1.APIResource{
		Name:       "users",
		Kind:       "User",
		Categories: []string{k9sCat},
	}
	m[client.NewGVR("groups")] = metav1.APIResource{
		Name:       "groups",
		Kind:       "Group",
		Categories: []string{k9sCat},
	}
}

func loadPreferred(f Factory, m ResourceMetas) error {
	if f.Client() == nil || !f.Client().ConnectionOK() {
		slog.Error("Load cluster resources - No API server connection")
		return nil
	}

	dial, err := f.Client().CachedDiscovery()
	if err != nil {
		return err
	}
	rr, err := dial.ServerPreferredResources()
	if err != nil {
		slog.Debug("Failed to load preferred resources", slogs.Error, err)
	}
	for _, r := range rr {
		for _, res := range r.APIResources {
			gvr := client.FromGVAndR(r.GroupVersion, res.Name)
			if isDeprecated(gvr) {
				continue
			}
			res.Group, res.Version = gvr.G(), gvr.V()
			if res.SingularName == "" {
				res.SingularName = strings.ToLower(res.Kind)
			}
			if !isStandardGroup(r.GroupVersion) {
				res.Categories = append(res.Categories, crdCat)
			}
			m[gvr] = res
		}
	}

	return nil
}

func isStandardGroup(gv string) bool {
	return stdGroups.Has(gv) || strings.Contains(gv, ".k8s.io")
}

var deprecatedGVRs = sets.New[client.GVR](
	client.NewGVR("extensions/v1beta1/ingresses"),
)

func isDeprecated(gvr client.GVR) bool {
	return deprecatedGVRs.Has(gvr)
}

// loadCRDs Wait for the cache to synced and then add some additional properties to CRD.
func loadCRDs(f Factory, m ResourceMetas) {
	if f.Client() == nil || !f.Client().ConnectionOK() {
		return
	}

	oo, err := f.List(crdGVR, client.ClusterScope, true, labels.Everything())
	if err != nil {
		slog.Warn("CRDs load Fail", slogs.Error, err)
		return
	}

	for _, o := range oo {
		var crd apiext.CustomResourceDefinition
		err = runtime.DefaultUnstructuredConverter.FromUnstructured(o.(*unstructured.Unstructured).Object, &crd)
		if err != nil {
			slog.Error("CRD conversion failed", slogs.Error, err)
			continue
		}
		gvr, version, ok := newGVRFromCRD(&crd)
		if !ok {
			continue
		}

		if meta, ok := m[gvr]; ok && version.Subresources != nil && version.Subresources.Scale != nil {
			if !slices.Contains(meta.Categories, scaleCat) {
				meta.Categories = append(meta.Categories, scaleCat)
				m[gvr] = meta
			}
		}
	}
}

func newGVRFromCRD(crd *apiext.CustomResourceDefinition) (client.GVR, apiext.CustomResourceDefinitionVersion, bool) {
	for _, v := range crd.Spec.Versions {
		if v.Served && !v.Deprecated {
			return client.NewGVRFromMeta(metav1.APIResource{
				Kind:    crd.Spec.Names.Kind,
				Group:   crd.Spec.Group,
				Name:    crd.Spec.Names.Plural,
				Version: v.Name,
			}), v, true
		}
	}

	return client.GVR{}, apiext.CustomResourceDefinitionVersion{}, false
}

func extractMeta(o runtime.Object) (metav1.APIResource, []error) {
	var (
		m    metav1.APIResource
		errs []error
	)

	crd, ok := o.(*unstructured.Unstructured)
	if !ok {
		return m, append(errs, fmt.Errorf("expected unstructured, but got %T", o))
	}

	var spec map[string]interface{}
	spec, errs = extractMap(crd.Object, "spec", errs)

	var meta map[string]interface{}
	meta, errs = extractMap(crd.Object, "metadata", errs)
	m.Name, errs = extractStr(meta, "name", errs)

	m.Group, errs = extractStr(spec, "group", errs)
	versions, errs := extractSlice(spec, "versions", errs)
	if len(versions) > 0 {
		m.Version = versions[0]
	}

	var scope string
	scope, errs = extractStr(spec, "scope", errs)

	m.Namespaced = isNamespaced(scope)

	var names map[string]interface{}
	names, errs = extractMap(spec, "names", errs)
	m.Kind, errs = extractStr(names, "kind", errs)
	m.SingularName, errs = extractStr(names, "singular", errs)
	m.Name, errs = extractStr(names, "plural", errs)
	m.ShortNames, errs = extractSlice(names, "shortNames", errs)

	return m, errs
}

func isNamespaced(scope string) bool {
	return scope == "Namespaced"
}

func extractSlice(m map[string]interface{}, n string, errs []error) ([]string, []error) {
	if m[n] == nil {
		return nil, errs
	}

	s, ok := m[n].([]string)
	if ok {
		return s, errs
	}

	ii, ok := m[n].([]interface{})
	if !ok {
		return s, append(errs, fmt.Errorf("failed to extract slice %s -- %#v", n, m))
	}

	ss := make([]string, 0, len(ii))
	for _, name := range ii {
		switch o := name.(type) {
		case string:
			ss = append(ss, o)
		case map[string]interface{}:
			s, ok := o["name"].(string)
			if ok {
				ss = append(ss, s)
			} else {
				errs = append(errs, fmt.Errorf("unable to find key %q in map", n))
			}
		default:
			errs = append(errs, fmt.Errorf("unknown field type %t for key %q", o, n))
		}
	}

	return ss, errs
}

func extractStr(m map[string]interface{}, n string, errs []error) (string, []error) {
	s, ok := m[n].(string)
	if !ok {
		return s, append(errs, fmt.Errorf("failed to extract string %s", n))
	}
	return s, errs
}

func extractMap(m map[string]interface{}, n string, errs []error) (map[string]interface{}, []error) {
	v, ok := m[n].(map[string]interface{})
	if !ok {
		return v, append(errs, fmt.Errorf("failed to extract field %s", n))
	}
	return v, errs
}
