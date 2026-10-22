package config

import (
	"strings"
	"testing"
)

const registrationFeature = `features:
  writesEnabled: true
  staticStorage: true
  podDefaults: true
`

func registrationStorage() Storage {
	return Storage{
		AllowedStorageClasses: []string{"nfs-static", "beegfs-static"},
		RegistrationTemplates: []RegistrationTemplate{
			{
				Name: "example-nfs", Kind: RegistrationKindNFS, StorageClass: "nfs-static",
				Capacity: "2400Gi", AccessModes: []string{"ReadWriteMany"},
				PVCRequest: "10Gi", PVCAccessModes: []string{"ReadWriteMany"},
				ReclaimPolicy: "Retain", VolumeMode: "Filesystem",
				NFSServer: "nfs.example.invalid", NFSRoots: []string{"/exports/kubeflow/alice"},
				MountOptions: []string{"nfsvers=3"}, RequireDirectoryUIDGID: true,
				PodDefaultName: "mount-toast", PodDefaultMountPathPrefix: "/home/jovyan",
			},
			{
				Name: "example-beegfs", Kind: RegistrationKindBeeGFS, StorageClass: "beegfs-static",
				Capacity: "500Gi", AccessModes: []string{"ReadWriteMany"},
				PVCRequest: "10Gi", PVCAccessModes: []string{"ReadWriteMany"},
				ReclaimPolicy: "Retain", VolumeMode: "Filesystem",
				BeeGFSDriver: BeeGFSDriverName, BeeGFSPrefix: "beegfs://beegfs.example.invalid/home",
				PodDefaultName: "mount-hpc-home", PodDefaultMountPathPrefix: "/home/jovyan",
			},
		},
	}
}

func TestRegistrationTemplateSchemaAndLoad(t *testing.T) {
	body := valid + registrationFeature + `storage:
  allowedStorageClasses: [nfs-static, beegfs-static]
  nfsTemplates: []
  csiTemplates: []
  registrationTemplates:
    - name: example-nfs
      backend: nfs
      storageClass: nfs-static
      pvCapacity: 2400Gi
      pvAccessModes: [ReadWriteMany]
      pvcRequest: 10Gi
      pvcAccessModes: [ReadWriteMany]
      reclaimPolicy: Retain
      volumeMode: Filesystem
      nfsServer: nfs.example.invalid
      nfsMountOptions: [nfsvers=3]
      nfsRoots: [/exports/kubeflow/REPLACE_WITH_APPROVED_USER_ROOT]
      requireDirectoryUIDGID: true
      podDefaultName: mount-toast
      podDefaultMountPathPrefix: /home/jovyan
    - name: example-beegfs
      backend: beegfs
      storageClass: beegfs-static
      pvCapacity: 500Gi
      pvAccessModes: [ReadWriteMany]
      pvcRequest: 10Gi
      pvcAccessModes: [ReadWriteMany]
      reclaimPolicy: Retain
      volumeMode: Filesystem
      beegfsDriver: beegfs.csi.netapp.com
      beegfsPrefix: beegfs://beegfs.example.invalid/home
      podDefaultName: mount-hpc-home
      podDefaultMountPathPrefix: /home/jovyan
`
	c, err := load(t, body)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Storage.RegistrationTemplates) != 2 {
		t.Fatalf("templates not loaded: %+v", c.Storage.RegistrationTemplates)
	}
	if c.Storage.RegistrationTemplates[0].Kind != RegistrationKindNFS || c.Storage.RegistrationTemplates[1].Kind != RegistrationKindBeeGFS {
		t.Fatal("backend spellings were not preserved")
	}
	if !c.Features.PodDefaults {
		t.Fatal("podDefaults feature not loaded")
	}
}

func TestRegistrationTemplateValidation(t *testing.T) {
	base, err := load(t, valid+registrationFeature)
	if err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*Storage){
		"duplicate legacy name": func(s *Storage) {
			s.NFSTemplates = []NFSTemplate{{Name: "example-nfs", Server: "nfs.example.invalid", BasePath: "/prepared", StorageClass: "nfs-static", AccessModes: []string{"ReadWriteMany"}}}
		},
		"unsupported backend":     func(s *Storage) { s.RegistrationTemplates[0].Kind = "cifs" },
		"class outside allowlist": func(s *Storage) { s.RegistrationTemplates[0].StorageClass = "other" },
		"empty class":             func(s *Storage) { s.RegistrationTemplates[0].StorageClass = "" },
		"missing pv capacity":     func(s *Storage) { s.RegistrationTemplates[0].Capacity = "" },
		"negative pv capacity":    func(s *Storage) { s.RegistrationTemplates[0].Capacity = "-1Gi" },
		"pvc request over pv":     func(s *Storage) { s.RegistrationTemplates[0].PVCRequest = "3000Gi" },
		"bad pv access mode":      func(s *Storage) { s.RegistrationTemplates[0].AccessModes = []string{"ReadWriteSometimes"} },
		"duplicate pv mode":       func(s *Storage) { s.RegistrationTemplates[0].AccessModes = []string{"ReadWriteMany", "ReadWriteMany"} },
		"pvc mode not in pv":      func(s *Storage) { s.RegistrationTemplates[0].PVCAccessModes = []string{"ReadWriteOnce"} },
		"delete reclaim":          func(s *Storage) { s.RegistrationTemplates[0].ReclaimPolicy = "Delete" },
		"block volume":            func(s *Storage) { s.RegistrationTemplates[0].VolumeMode = "Block" },
		"nfs root slash":          func(s *Storage) { s.RegistrationTemplates[0].NFSRoots = []string{"/"} },
		"nfs relative root":       func(s *Storage) { s.RegistrationTemplates[0].NFSRoots = []string{"exports/alice"} },
		"nfs unclean root":        func(s *Storage) { s.RegistrationTemplates[0].NFSRoots = []string{"/exports/../alice"} },
		"nfs duplicate root":      func(s *Storage) { s.RegistrationTemplates[0].NFSRoots = []string{"/exports/alice", "/exports/alice"} },
		"nfs no root":             func(s *Storage) { s.RegistrationTemplates[0].NFSRoots = nil },
		"nfs no server":           func(s *Storage) { s.RegistrationTemplates[0].NFSServer = "" },
		"nfs bad mount option":    func(s *Storage) { s.RegistrationTemplates[0].MountOptions = []string{"nfsvers = 3"} },
		"nfs uid optional":        func(s *Storage) { s.RegistrationTemplates[0].RequireDirectoryUIDGID = false },
		"beegfs wrong driver":     func(s *Storage) { s.RegistrationTemplates[1].BeeGFSDriver = "other.csi" },
		"beegfs root prefix":      func(s *Storage) { s.RegistrationTemplates[1].BeeGFSPrefix = "beegfs://beegfs.example.invalid/" },
		"beegfs query":            func(s *Storage) { s.RegistrationTemplates[1].BeeGFSPrefix = "beegfs://beegfs.example.invalid/home?x=1" },
		"beegfs percent":          func(s *Storage) { s.RegistrationTemplates[1].BeeGFSPrefix = "beegfs://beegfs.example.invalid/ho%6de" },
		"beegfs userinfo": func(s *Storage) {
			s.RegistrationTemplates[1].BeeGFSPrefix = "beegfs://user@beegfs.example.invalid/home"
		},
		"mixed nfs beegfs":     func(s *Storage) { s.RegistrationTemplates[1].NFSServer = "nfs.example.invalid" },
		"poddefault name only": func(s *Storage) { s.RegistrationTemplates[0].PodDefaultMountPathPrefix = "" },
		"poddefault path only": func(s *Storage) { s.RegistrationTemplates[0].PodDefaultName = "" },
		"poddefault name 53":   func(s *Storage) { s.RegistrationTemplates[0].PodDefaultName = strings.Repeat("a", 53) },
		"poddefault relative":  func(s *Storage) { s.RegistrationTemplates[0].PodDefaultMountPathPrefix = "home/jovyan" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			cfg := base
			cfg.Storage = registrationStorage()
			mutate(&cfg.Storage)
			if cfg.Validate() == nil {
				t.Fatal("unsafe registration template accepted")
			}
		})
	}
}

func TestPodDefaultsFeatureRequiresWrites(t *testing.T) {
	if _, err := load(t, valid+"features:\n  podDefaults: true\n"); err == nil {
		t.Fatal("podDefaults accepted without writesEnabled")
	}
}

func TestLegacyTemplatesRemainIndependent(t *testing.T) {
	cfg, err := load(t, valid+`features:
  writesEnabled: true
  staticStorage: true
storage:
  allowedStorageClasses: [approved]
  nfsTemplates:
    - name: legacy-nfs
      server: nfs.example.invalid
      basePath: /prepared
      storageClass: approved
      accessModes: [ReadWriteMany]
      mountOptions: [nfsvers=3]
  csiTemplates: []
  registrationTemplates: []
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Storage.RegistrationTemplates) != 0 || len(cfg.Storage.NFSTemplates) != 1 {
		t.Fatal("legacy template list was reinterpreted")
	}
}
