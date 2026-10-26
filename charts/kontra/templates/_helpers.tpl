{{/* The chart's name, and the release's full name (the prefix of every object). */}}
{{- define "kontra.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "kontra.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/* Labels; call with (dict "ctx" $ "component" "gate"). */}}
{{- define "kontra.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kontra.name" .ctx }}
app.kubernetes.io/instance: {{ .ctx.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end }}

{{- define "kontra.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .ctx.Chart.Name .ctx.Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "kontra.selectorLabels" . }}
app.kubernetes.io/version: {{ .ctx.Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .ctx.Release.Service }}
app.kubernetes.io/part-of: kontra
{{- end }}

{{/* Object names. */}}
{{- define "kontra.gate" -}}{{ include "kontra.fullname" . }}-gate{{- end }}
{{- define "kontra.postgres" -}}{{ include "kontra.fullname" . }}-postgres{{- end }}
{{- define "kontra.temporal" -}}{{ include "kontra.fullname" . }}-temporal{{- end }}

{{- define "kontra.configSecret" -}}
{{- default (printf "%s-config" (include "kontra.fullname" .)) .Values.config.existingSecret }}
{{- end }}

{{- define "kontra.postgresSecret" -}}
{{- default (include "kontra.postgres" .) .Values.postgres.auth.existingSecret }}
{{- end }}

{{- define "kontra.postgresSecretKey" -}}
{{- if .Values.postgres.auth.existingSecret }}{{ .Values.postgres.auth.existingSecretKey }}{{ else }}password{{ end }}
{{- end }}

{{- define "kontra.gateImage" -}}
{{- if .Values.image.digest }}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest }}
{{- else }}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) }}
{{- end }}
{{- end }}

{{/*
The restricted Pod Security Standard, for every pod and every container; call with the uid. The volumes
the pods mount (secret, emptyDir, persistentVolumeClaim) are all the standard allows.
*/}}
{{- define "kontra.podSecurityContext" -}}
runAsNonRoot: true
runAsUser: {{ . }}
runAsGroup: {{ . }}
fsGroup: {{ . }}
fsGroupChangePolicy: OnRootMismatch
seccompProfile:
  type: RuntimeDefault
{{- end }}

{{- define "kontra.containerSecurityContext" -}}
runAsNonRoot: true
runAsUser: {{ . }}
runAsGroup: {{ . }}
privileged: false
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
capabilities:
  drop: [ALL]
seccompProfile:
  type: RuntimeDefault
{{- end }}

{{/* What a pod of this release is scheduled by. */}}
{{- define "kontra.scheduling" -}}
{{- with .Values.nodeSelector }}
nodeSelector:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .Values.tolerations }}
tolerations:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .Values.affinity }}
affinity:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .Values.imagePullSecrets }}
imagePullSecrets:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- end }}

{{/* The gate's env, for the migrate init container and the gate alike. */}}
{{- define "kontra.gateEnv" -}}
# os.UserCacheDir: the ACME cache's default directory lands on the volume, not the read-only root.
- name: XDG_CACHE_HOME
  value: /var/lib/kontra-gate
{{- if .Values.postgres.enabled }}
# pgx takes the password from PGPASSWORD when kontra.yaml's postgres URL carries none.
- name: PGPASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ include "kontra.postgresSecret" . }}
      key: {{ include "kontra.postgresSecretKey" . }}
{{- end }}
{{- with .Values.gate.env }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{/* Refuse values that cannot make a gate. */}}
{{- define "kontra.validate" -}}
{{- if and .Values.config.existingSecret .Values.config.kontraYaml }}
{{- fail "config: set existingSecret or kontraYaml, not both" }}
{{- end }}
{{- if not (or .Values.config.existingSecret .Values.config.kontraYaml) }}
{{- fail "config: set config.existingSecret (a Secret holding kontra.yaml) or config.kontraYaml (see values.yaml)" }}
{{- end }}
{{- if and .Values.ingressRouteTCP.enabled (not .Values.ingressRouteTCP.hosts) }}
{{- fail "ingressRouteTCP.hosts: name at least one host (HostSNI)" }}
{{- end }}
{{- if gt (int .Values.gate.replicas) 1 }}
{{- if .Values.persistence.enabled }}
{{- fail "gate.replicas above 1 with persistence.enabled: the ACME cache volume is one replica's (D70, D183); set persistence.enabled=false and use a provided certificate or a TLS-terminating front" }}
{{- end }}
{{- if not .Values.gate.peer.enabled }}
{{- fail "gate.replicas above 1 needs gate.peer.enabled: without the peer hop an MCP session pinned to another replica's tunnel ends with a 404 (spec/relay.md §11.2, D179, D183)" }}
{{- end }}
{{- end }}
{{- if lt (int .Values.gate.replicas) 1 }}
{{- fail "gate.replicas: at least 1" }}
{{- end }}
{{- if and .Values.gate.peer.enabled (not .Values.gate.peer.secret.existingSecret) }}
{{- fail "gate.peer.secret.existingSecret: required with gate.peer.enabled (a Secret holding the shared secret, at least 32 bytes)" }}
{{- end }}
{{- end }}
