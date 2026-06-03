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

{{/* The Secret name workloads reference (existing or chart-managed). */}}
{{- define "zk-drive.secretName" -}}
{{- if .Values.secrets.existingSecret -}}
{{- .Values.secrets.existingSecret -}}
{{- else -}}
zk-drive-secrets
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

{{/* envFrom block shared by all app workloads. */}}
{{- define "zk-drive.envFrom" -}}
envFrom:
  - configMapRef:
      name: zk-drive-config
  - secretRef:
      name: {{ include "zk-drive.secretName" . }}
{{- end -}}
