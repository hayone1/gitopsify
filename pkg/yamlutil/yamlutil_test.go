package yamlutil

import "testing"

func TestMarshalOrderAndBlocks(t *testing.T) {
	obj := map[string]any{
		"spec": map[string]any{"containers": []any{map[string]any{"image": "nginx", "name": "web", "args": []any{"a"}}}},
		"data": map[string]any{"script.sh": "#!/bin/sh\necho hi\n", "flag": "true", "n": "1.0"},
		"metadata": map[string]any{
			"annotations": map[string]any{"b": "2"},
			"labels":      map[string]any{"a": "1"},
			"namespace":   "shop",
			"name":        "web",
		},
		"kind":       "ConfigMap",
		"apiVersion": "v1",
	}
	b, err := Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	want := `apiVersion: v1
kind: ConfigMap
metadata:
  name: web
  namespace: shop
  labels:
    a: "1"
  annotations:
    b: "2"
spec:
  containers:
    - name: web
      args:
        - a
      image: nginx
data:
  flag: "true"
  "n": "1.0"
  script.sh: |
    #!/bin/sh
    echo hi
`
	if string(b) != want {
		t.Errorf("got:\n%s\nwant:\n%s", b, want)
	}
}

func TestMarshalDocs(t *testing.T) {
	b, err := MarshalDocs(map[string]any{"a": 1}, map[string]any{"b": 2.5})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "a: 1\n---\nb: 2.5\n" {
		t.Errorf("got %q", b)
	}
}
