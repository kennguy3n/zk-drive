{{/*
Common helpers for the zk-drive chart.
*/}}

{{/* Namespace the chart deploys into. */}}
{{- define "zk-drive.namespace" -}}
{{- .Values.namespace.name | default .Release.Namespace -}}
{{- end -}}

{{/* Standard labels stamped on every object. */}}
{{- define "zk-drive.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/name: zk-drive
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
{{- end -}}

{{/* Fully-qualified image reference. */}}
{{- define "zk-drive.image" -}}
{{- printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion) -}}
{{- end -}}

{{/*
The Secret name workloads reference (existing or chart-managed).
Fails fast when secrets.create=false without an existingSecret: the
workloads + migrate Job envFrom this name, so a missing Secret would
otherwise surface as a runtime CreateContainerConfigError instead of a
clear install-time error.
*/}}
{{- define "zk-drive.secretName" -}}
{{- if .Values.secrets.existingSecret -}}
{{- .Values.secrets.existingSecret -}}
{{- else if .Values.secrets.create -}}
zk-drive-secrets
{{- else -}}
{{- fail "secrets.create=false requires secrets.existingSecret to be set: the server, worker, and migrate Job envFrom a Secret, so an existing one must be referenced (or set secrets.create=true to have the chart manage it)." -}}
{{- end -}}
{{- end -}}

{{/* imagePullSecrets block. */}}
{{- define "zk-drive.imagePullSecrets" -}}
{{- with .Values.imagePullSecrets }}
imagePullSecrets:
{{- range . }}
  - name: {{ . }}
{{- end }}
{{- end }}
{{- end -}}

{{/*
Egress rule to a backend dependency (Postgres / NATS / ClamAV).
When the in-cluster dependency is enabled, egress is scoped to its pod via
podSelector (tight). When it is disabled — i.e. a managed/external endpoint
(RDS, Cloud SQL, NGS, managed ClamAV) with no in-cluster pod to select — the
chart falls back to a port-only rule so traffic can reach the managed
endpoint's IP under default-deny, mirroring the Redis/S3 egress rules.
Usage:
  {{- include "zk-drive.backendEgress" (dict "enabled" .Values.postgres.enabled "app" "postgres" "port" .Values.networkPolicy.postgresPort) | nindent 4 }}
*/}}
{{- define "zk-drive.backendEgress" -}}
{{- if .enabled -}}
- to:
    - podSelector:
        matchLabels:
          app: {{ .app }}
  ports:
    - protocol: TCP
      port: {{ .port }}
{{- else -}}
- ports:
    - protocol: TCP
      port: {{ .port }}
{{- end -}}
{{- end -}}

{{/* envFrom block shared by all app workloads. */}}
{{- define "zk-drive.envFrom" -}}
envFrom:
  - configMapRef:
      name: zk-drive-config
  - secretRef:
      name: {{ include "zk-drive.secretName" . }}
{{- end -}}
