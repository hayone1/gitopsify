{{- define "gitopsify.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "gitopsify.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "gitopsify.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{ include "gitopsify.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "gitopsify.selectorLabels" -}}
app.kubernetes.io/name: {{ include "gitopsify.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "gitopsify.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "gitopsify.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "gitopsify.secretName" -}}
{{- default (include "gitopsify.fullname" .) .Values.secrets.existingSecret -}}
{{- end -}}

{{/* The Server resource: values.server plus what the chart decides. */}}
{{- define "gitopsify.serverConfig" -}}
{{- $spec := deepCopy .Values.server -}}
{{- $_ := set $spec "listen" ":8080" -}}
{{- $_ := set $spec "dataDir" "/var/lib/gitopsify" -}}
{{- $auth := default dict $spec.auth -}}
{{- $_ := set $auth "sessionKey" (dict "env" "GITOPSIFY_SESSION_KEY") -}}
{{- if eq (default "oidc" $auth.mode) "oidc" -}}
{{- if not (and $auth.oidc $auth.oidc.issuer) -}}
{{- fail "server.auth.oidc.issuer is required (or set server.auth.mode: local, which only suits gitopsify ui on a laptop)" -}}
{{- end -}}
{{- if not $spec.externalURL -}}
{{- fail "server.externalURL is required with OIDC: the address people reach gitopsify at" -}}
{{- end -}}
{{- $_ := set $auth.oidc "clientSecret" (dict "env" "GITOPSIFY_OIDC_CLIENT_SECRET") -}}
{{- end -}}
{{- $_ := set $spec "auth" $auth -}}
{{- $clusters := list -}}
{{- if .Values.inCluster.enabled -}}
{{- $clusters = append $clusters (dict "name" .Values.inCluster.name "description" .Values.inCluster.description "impersonate" .Values.inCluster.impersonate "access" (dict "inCluster" true) "sealedSecrets" .Values.inCluster.sealedSecrets) -}}
{{- end -}}
{{- range .Values.server.clusters -}}
{{- $clusters = append $clusters . -}}
{{- end -}}
{{- $_ := set $spec "clusters" $clusters -}}
{{- if eq .Values.storage.type "postgres" -}}
{{- $pg := dict "url" (dict "env" "GITOPSIFY_POSTGRES_URL") -}}
{{- if .Values.storage.postgres.maxConnections -}}
{{- $_ := set $pg "maxConnections" .Values.storage.postgres.maxConnections -}}
{{- end -}}
{{- $_ := set $spec "storage" (dict "type" "postgres" "postgres" $pg) -}}
{{- if gt (int .Values.replicaCount) 1 -}}
{{- $providers := default dict $spec.providers -}}
{{- if not (hasKey $providers "disableInstall") -}}
{{- $_ := set $providers "disableInstall" true -}}
{{- end -}}
{{- $_ := set $spec "providers" $providers -}}
{{- end -}}
{{- else if gt (int .Values.replicaCount) 1 -}}
{{- fail "replicaCount > 1 needs storage.type: postgres (with file storage the data directory belongs to one pod)" -}}
{{- end -}}
apiVersion: gitopsify.io/v1alpha1
kind: Server
metadata:
  name: {{ include "gitopsify.fullname" . }}
spec:
{{ toYaml $spec | indent 2 }}
{{- end -}}
