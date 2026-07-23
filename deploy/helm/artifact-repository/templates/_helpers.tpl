{{- define "artifact-repository.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "artifact-repository.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "artifact-repository.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "artifact-repository.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
app.kubernetes.io/name: {{ include "artifact-repository.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "artifact-repository.selectorLabels" -}}
app.kubernetes.io/name: {{ include "artifact-repository.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "artifact-repository.image" -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}

{{- define "artifact-repository.secretName" -}}
{{- if .Values.secrets.create -}}
{{- include "artifact-repository.fullname" . -}}
{{- else -}}
{{- required "secrets.existingSecret is required when secrets.create=false" .Values.secrets.existingSecret -}}
{{- end -}}
{{- end -}}

{{- define "artifact-repository.storageClaimName" -}}
{{- if .Values.storage.filesystem.persistence.existingClaim -}}
{{- .Values.storage.filesystem.persistence.existingClaim -}}
{{- else -}}
{{- printf "%s-data" (include "artifact-repository.fullname" .) -}}
{{- end -}}
{{- end -}}
