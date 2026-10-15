package web

import (
	"kubeflow-admin/internal/model"
	"strings"
)

// Storage classes and unclaimed Available volumes are cluster-scoped choices.
// Retain the selected namespace's bound PVs through both sides of the binding,
// even while one side's inventory has not caught up with the other.
func matchesNamespace(r model.Resource, namespace string, all []model.Resource) bool {
	if namespace == "" || r.Namespace == namespace {
		return true
	}
	switch strings.ToLower(r.Kind) {
	case "profile", "namespace":
		return r.Name == namespace
	case "storageclass":
		return true
	case "persistentvolume", "pv":
		if strings.HasPrefix(r.Details["claim"], namespace+"/") || (r.State == "Available" && r.Details["claim"] == "") {
			return true
		}
		for _, claim := range all {
			if strings.EqualFold(claim.Kind, "PersistentVolumeClaim") && claim.Namespace == namespace && claim.Details["volume"] == r.Name {
				return true
			}
		}
	}
	return false
}
