package match

import "testing"

func TestKind(t *testing.T) {
	dep := KindInfo{Group: "apps", Kind: "Deployment", Resource: "deployments"}
	ing := KindInfo{Group: "networking.k8s.io", Kind: "Ingress", Resource: "ingresses", ShortNames: []string{"ing"}}
	svc := KindInfo{Kind: "Service", Resource: "services"}
	for _, c := range []struct {
		pattern string
		k       KindInfo
		want    bool
	}{
		{"Deployment", dep, true}, {"deployments", dep, true}, {"deploy", dep, true},
		{"deployment.apps", dep, true}, {"deployment.batch", dep, false},
		{"ing", ing, true}, {"Ingress.networking.k8s.io", ing, true}, {"ingress.extensions", ing, false},
		{"svc", svc, true}, {"service.core", svc, true}, {"service.v1", svc, true}, {"Pod", svc, false},
	} {
		if got := Kind(c.pattern, c.k); got != c.want {
			t.Errorf("Kind(%q, %s) = %v", c.pattern, c.k.Kind, got)
		}
	}
}

func TestGlob(t *testing.T) {
	if !Glob([]string{"shop-*"}, "shop-eu") || Glob([]string{"shop-*"}, "shop") || Glob(nil, "x") || !GlobOrEmpty(nil, "x") {
		t.Error("glob mismatch")
	}
}
