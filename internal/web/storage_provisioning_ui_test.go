package web

import (
	"encoding/json"
	"strings"
	"testing"

	"kubeflow-admin/internal/config"
	"kubeflow-admin/internal/model"
)

func registrationUIConfig() config.Config {
	return config.Config{
		Features: config.Features{WritesEnabled: true, StaticStorage: true, PodDefaults: true},
		Storage: config.Storage{
			AllowedStorageClasses: []string{"nfs-csi", "beegfs-csi"},
			RegistrationTemplates: []config.RegistrationTemplate{
				{
					Name: "nfs-template", Kind: config.RegistrationKindNFS, StorageClass: "nfs-csi",
					Capacity: "2400Gi", AccessModes: []string{"ReadWriteMany"},
					PVCRequest: "10Gi", PVCAccessModes: []string{"ReadWriteMany"},
					ReclaimPolicy: "Retain", VolumeMode: "Filesystem",
					NFSServer: "nfs.example.test", NFSRoots: []string{"/exports/kubeflow"},
					MountOptions: []string{"nfsvers=3"}, RequireDirectoryUIDGID: true,
					PodDefaultName: "mount-toast", PodDefaultMountPathPrefix: "/home/jovyan",
				},
				{
					Name: "beegfs-template", Kind: config.RegistrationKindBeeGFS, StorageClass: "beegfs-csi",
					Capacity: "500Gi", AccessModes: []string{"ReadWriteMany"},
					PVCRequest: "10Gi", PVCAccessModes: []string{"ReadWriteMany"},
					ReclaimPolicy: "Retain", VolumeMode: "Filesystem",
					BeeGFSDriver: "beegfs.csi.netapp.com", BeeGFSPrefix: "beegfs://beegfs.example.test/home",
				},
			},
		},
	}
}

func TestPVReclaimAcknowledgementIsSeparatelyGated(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, writes := range []bool{false, true} {
			for _, deletion := range []bool{false, true} {
				s, _, _ := fixture()
				s.cfg.Features.PVReclaim = enabled
				s.cfg.Features.WritesEnabled = writes
				s.cfg.Features.StorageDeletion = deletion
				body := request(handler(s), "GET", "/storage", "").Body.String()
				got := strings.Contains(body, `id="storage-delete-reclaim-ack"`)
				if got != (enabled && writes && deletion) {
					t.Fatalf("reclaim projection enabled=%v writes=%v deletion=%v: %v", enabled, writes, deletion, got)
				}
			}
		}
	}
}

func TestStorageRegistrationInventoryProjection(t *testing.T) {
	s, k, _ := fixture()
	s.cfg = registrationUIConfig()
	k.result.StorageTemplates = []model.StorageRegistrationTemplate{
		{
			Name: "nfs-template", Kind: "nfs", StorageClass: "nfs-csi",
			PVCapacity: "2400Gi", PVAccessModes: []string{"ReadWriteMany"},
			PVCRequest: "10Gi", PVCAccessModes: []string{"ReadWriteMany"},
			ReclaimPolicy: "Retain", VolumeMode: "Filesystem",
			NFSServer: "nfs.example.test", NFSRoots: []string{"/exports/kubeflow"},
			NFSMountOptions: []string{"nfsvers=3"}, RequireDirectoryUIDGID: true,
			PodDefaultName: "mount-toast", PodDefaultMountPathPrefix: "/home/jovyan",
		},
		{
			Name: "beegfs-template", Kind: "beegfs", StorageClass: "beegfs-csi",
			PVCapacity: "500Gi", PVAccessModes: []string{"ReadWriteMany"},
			PVCRequest: "10Gi", PVCAccessModes: []string{"ReadWriteMany"},
			ReclaimPolicy: "Retain", VolumeMode: "Filesystem",
			BeeGFSDriver: "beegfs.csi.netapp.com", BeeGFSPrefix: "beegfs://beegfs.example.test/home",
		},
	}
	response := request(handler(s), "GET", "/api/v1/inventory", "")
	if response.Code != 200 {
		t.Fatalf("inventory status=%d", response.Code)
	}
	var got model.Inventory
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.StorageTemplates) != 2 {
		t.Fatalf("registration template projection=%+v", got.StorageTemplates)
	}
	nfs := got.StorageTemplates[0]
	if nfs.Name != "nfs-template" || nfs.Kind != "nfs" || nfs.StorageClass != "nfs-csi" ||
		nfs.PVCapacity != "2400Gi" || nfs.PVCRequest != "10Gi" || !nfs.RequireDirectoryUIDGID ||
		nfs.PodDefaultName != "mount-toast" || nfs.PodDefaultMountPathPrefix != "/home/jovyan" {
		t.Fatalf("NFS projection changed: %+v", nfs)
	}
	// The web layer passes the backend projection through unchanged. It never
	// duplicates it from configuration or invents a second bootstrap format.
	k.result.StorageTemplates = nil
	response = request(handler(s), "GET", "/api/v1/inventory", "")
	got = model.Inventory{}
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.StorageTemplates) != 0 {
		t.Fatalf("web layer re-derived an inventory projection: %+v", got.StorageTemplates)
	}
}

func TestStorageRegistrationPageProjection(t *testing.T) {
	s, _, _ := fixture()
	s.cfg = registrationUIConfig()
	body := request(handler(s), "GET", "/storage", "").Body.String()
	// The page only carries the capability and the gated action labels. The
	// template values are fetched once from inventory.storageTemplates and are
	// never bootstrapped into a second competing format.
	if strings.Contains(body, "storage-template-registry") {
		t.Fatal("the page still carries a duplicate template bootstrap")
	}
	if !strings.Contains(body, `name="pod-defaults" content="true"`) {
		t.Fatal("the PodDefault capability is not projected")
	}
	if !strings.Contains(body, `value="pv.create.static"`) || !strings.Contains(body, `value="pvc.create.bound"`) {
		t.Fatal("the registration actions are not offered")
	}
	if !strings.Contains(body, `id="storage-registration-template"`) {
		t.Fatal("the registration template selector is missing")
	}
	// Bound forms never carry backend path inputs; the selected PV already
	// owns the fixed path.
	for _, input := range []string{`name="nfsPath"`, `name="beeGFSPath"`} {
		index := strings.Index(body, input)
		if index < 0 {
			t.Fatalf("missing %s input", input)
		}
		labelStart := strings.LastIndex(body[:index], "<label ")
		if labelStart < 0 || !strings.Contains(body[labelStart:index], `data-storage="pv.create.static"`) {
			t.Fatalf("%s is not limited to a new PV", input)
		}
	}
	if !strings.Contains(body, `id="storage-pod-default-name"`) || !strings.Contains(body, `maxlength="52"`) {
		t.Fatal("the PodDefault creation name is not bounded to 52 bytes")
	}
	if !strings.Contains(body, `id="storage-delete-pod-default-ack"`) {
		t.Fatal("the related PodDefault cleanup acknowledgement is missing")
	}
}

func TestStorageRegistrationPageGatedOff(t *testing.T) {
	s, _, _ := fixture()
	s.cfg = registrationUIConfig()
	s.cfg.Features.WritesEnabled = false
	s.cfg.Features.StaticStorage = false
	s.cfg.Features.PodDefaults = false
	body := request(handler(s), "GET", "/storage", "").Body.String()
	if strings.Contains(body, `value="pv.create.static"`) || strings.Contains(body, `value="pvc.create.bound"`) {
		t.Fatal("registration actions are offered without writes and static storage")
	}
	if !strings.Contains(body, `name="pod-defaults" content="false"`) {
		t.Fatal("disabled PodDefault capability is not projected as false")
	}
}

func TestStorageKindActionsRequireConfiguredTemplates(t *testing.T) {
	const (
		legacyAction   = `value="pv.nfs.create"`
		reserveAction  = `value="pv.reserve"`
		staticPVAction = `value="pv.create.static"`
		boundPVCAction = `value="pvc.create.bound"`
	)
	nfs := []config.NFSTemplate{{Name: "legacy-nfs", StorageClass: "nfs-csi", AccessModes: []string{"ReadWriteMany"}}}
	csi := []config.CSITemplate{{Name: "legacy-csi", StorageClass: "nfs-csi", AccessModes: []string{"ReadWriteMany"}}}
	registration := []config.RegistrationTemplate{{Name: "registration-nfs", Kind: config.RegistrationKindNFS, StorageClass: "nfs-csi"}}
	for _, tc := range []struct {
		name             string
		nfs              []config.NFSTemplate
		csi              []config.CSITemplate
		registration     []config.RegistrationTemplate
		wantLegacy       bool
		wantRegistration bool
	}{
		{"no templates", nil, nil, nil, false, false},
		{"approved NFS template", nfs, nil, nil, true, false},
		{"approved CSI template", nil, csi, nil, true, false},
		{"registration template only", nil, nil, registration, false, true},
		{"legacy and registration templates", nfs, nil, registration, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := fixture()
			s.cfg = config.Config{
				Features: config.Features{WritesEnabled: true, StaticStorage: true},
				Storage: config.Storage{
					NFSTemplates:          tc.nfs,
					CSITemplates:          tc.csi,
					RegistrationTemplates: tc.registration,
				},
			}
			body := request(handler(s), "GET", "/storage", "").Body.String()
			if !strings.Contains(body, reserveAction) {
				t.Fatal("the template-independent reserve action is missing")
			}
			if got := strings.Contains(body, legacyAction); got != tc.wantLegacy {
				t.Fatalf("legacy template action offered=%v, want %v", got, tc.wantLegacy)
			}
			for _, action := range []string{staticPVAction, boundPVCAction} {
				if got := strings.Contains(body, action); got != tc.wantRegistration {
					t.Fatalf("registration action %s offered=%v, want %v", action, got, tc.wantRegistration)
				}
			}
		})
	}
}
