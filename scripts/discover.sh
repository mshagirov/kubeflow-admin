#!/usr/bin/env bash
# User-run only. Read-only selections; never exports Secrets, env, or raw objects.
set -euo pipefail
if [[ $# -ne 1 || $1 == --help ]]; then
  echo "Usage: bash scripts/discover.sh EXISTING_KUBECTL_CONTEXT > discovery.txt" >&2
  echo 'Review names, addresses, identity and infrastructure metadata before sharing.' >&2
  exit 2
fi
context=$1
k() { kubectl --context "$context" --request-timeout=20s "$@"; }
section() { printf '\n## %s\n' "$1"; }
# Optional APIs may be absent; errors stay visible and the remaining sections run.
optional() { if ! k "$@"; then echo 'Unavailable/forbidden; review locally.' >&2; fi; }
section 'Kubernetes client/server version'
k version -o json
section 'Node selection metadata (no addresses or full labels)'
optional get nodes -o 'custom-columns=NAME:.metadata.name,HOSTNAME:.metadata.labels.kubernetes\.io/hostname,KUBELET:.status.nodeInfo.kubeletVersion,ARCH:.status.nodeInfo.architecture,OS:.status.nodeInfo.osImage'
section 'Served Kubeflow APIs'
k api-resources --api-group=kubeflow.org
section 'CRD served/storage versions (no schemas or examples)'
optional get crd profiles.kubeflow.org notebooks.kubeflow.org -o 'custom-columns=NAME:.metadata.name,VERSIONS:.spec.versions[*].name,SERVED:.spec.versions[*].served,STORAGE:.spec.versions[*].storage'
section 'Controller images (no arguments or environment)'
optional get deployments -A -o 'custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name,IMAGES:.spec.template.spec.containers[*].image'
section 'Storage classes'
optional get storageclasses.storage.k8s.io -o 'custom-columns=NAME:.metadata.name,DRIVER:.provisioner,BINDING:.volumeBindingMode,RECLAIM:.reclaimPolicy,EXPANSION:.allowVolumeExpansion'
section 'CSI drivers'
optional get csidrivers.storage.k8s.io -o 'custom-columns=NAME:.metadata.name,ATTACH:.spec.attachRequired,MODES:.spec.volumeLifecycleModes'
section 'PV topology/capacity (no export paths or CSI handles)'
optional get pv -o 'custom-columns=NAME:.metadata.name,CLASS:.spec.storageClassName,CAPACITY:.spec.capacity.storage,MODES:.spec.accessModes,PHASE:.status.phase,RECLAIM:.spec.persistentVolumeReclaimPolicy'
section 'PVC bindings'
optional get pvc -A -o 'custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name,CLASS:.spec.storageClassName,REQUEST:.spec.resources.requests.storage,PHASE:.status.phase,PV:.spec.volumeName'
section 'Profiles (no owner identities)'
optional get profiles.kubeflow.org -o 'custom-columns=NAME:.metadata.name,API:.apiVersion'
section 'Notebook names and images (no literal env/annotations/spec dumps)'
optional get notebooks.kubeflow.org -A -o 'custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name,API:.apiVersion,IMAGES:.spec.template.spec.containers[*].image'
section 'Ingress services'
optional get services -A --field-selector spec.type=LoadBalancer -o 'custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name,PORTS:.spec.ports[*].port'
section 'Ingress classes'
optional get ingressclasses.networking.k8s.io -o 'custom-columns=NAME:.metadata.name,CONTROLLER:.spec.controller'
section 'Istio gateway routing selections (no TLS Secret content)'
optional get gateways.networking.istio.io -A -o 'custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name,SELECTOR:.spec.selector,HOSTS:.spec.servers[*].hosts,PORTS:.spec.servers[*].port.number'
section 'Istio VirtualService hosts'
optional get virtualservices.networking.istio.io -A -o 'custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name,HOSTS:.spec.hosts,GATEWAYS:.spec.gateways'
