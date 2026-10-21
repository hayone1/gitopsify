package target

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/kube"
	"github.com/hayone1/gitopsify/pkg/model"
)

// What to do with Helm release records in the target.
const (
	HelmRecordsKeep         = "keep"
	HelmRecordsBackup       = "backup"
	HelmRecordsBackupDelete = "backup-delete"
)

// AdoptOptions configure Adopt.
type AdoptOptions struct {
	HelmRecords string
	BackupDir   string
	DryRun      bool
	Log         func(format string, args ...any)
}

// AdoptResult lists what Adopt did.
type AdoptResult struct {
	BackedUp []string
	Deleted  []string
	Kept     []string
}

// StaleReleaseRecords reports whether a release's Helm records become stale
// once the GitOps tool manages it: Argo CD renders charts itself and plain
// manifests bypass Helm, while a Flux HelmRelease keeps using the records.
func StaleReleaseRecords(cfg *config.Config, a *model.App) bool {
	return !(cfg.Output.Tool == config.ToolFlux && a.IsHelm())
}

// Adopt prepares an environment's cluster for the takeover of objects that
// already run there.
func Adopt(ctx context.Context, inv *model.Inventory, cfg *config.Config, env *config.Environment, c *kube.Client, o AdoptOptions) (*AdoptResult, error) {
	log := o.Log
	if log == nil {
		log = func(string, ...any) {}
	}
	res := &AdoptResult{}
	for _, a := range inv.Apps {
		if a.Release == nil {
			continue
		}
		rel := a.Release
		list, err := c.Kubernetes.CoreV1().Secrets(rel.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "owner=helm,name=" + rel.Name})
		if err != nil {
			return res, fmt.Errorf("listing the records of Helm release %s: %w", rel.Key(), err)
		}
		if len(list.Items) == 0 {
			continue
		}
		if !StaleReleaseRecords(cfg, a) || o.HelmRecords == HelmRecordsKeep {
			res.Kept = append(res.Kept, rel.Key())
			continue
		}
		for i := range list.Items {
			s := &list.Items[i]
			name := s.Namespace + "/" + s.Name
			if o.BackupDir != "" {
				obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(s)
				if err != nil {
					return res, err
				}
				obj["apiVersion"], obj["kind"] = "v1", "Secret"
				b, err := yaml.Marshal(obj)
				if err != nil {
					return res, err
				}
				p := filepath.Join(o.BackupDir, s.Namespace, s.Name+".yaml")
				if !o.DryRun {
					if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
						return res, err
					}
					if err := os.WriteFile(p, b, 0o600); err != nil {
						return res, err
					}
				}
				log("backed up %s to %s", name, p)
				res.BackedUp = append(res.BackedUp, name)
			}
			if o.HelmRecords == HelmRecordsBackupDelete {
				if !o.DryRun {
					if err := c.Kubernetes.CoreV1().Secrets(s.Namespace).Delete(ctx, s.Name, metav1.DeleteOptions{}); err != nil {
						return res, fmt.Errorf("deleting %s: %w", name, err)
					}
				}
				log("deleted %s", name)
				res.Deleted = append(res.Deleted, name)
			}
		}
	}
	return res, nil
}
