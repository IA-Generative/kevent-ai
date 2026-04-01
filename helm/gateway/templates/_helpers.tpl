{{/*
Expand the name of the chart.
*/}}
{{- define "kevent-gateway.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "kevent-gateway.fullname" -}}
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

{{/*
Create chart label.
*/}}
{{- define "kevent-gateway.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "kevent-gateway.labels" -}}
helm.sh/chart: {{ include "kevent-gateway.chart" . }}
{{ include "kevent-gateway.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "kevent-gateway.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kevent-gateway.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Adresse Redis.
HAProxy du subchart redis-ha expose le master actif sur un endpoint stable.
Service créé par redis-ha : {{ .Release.Name }}-redis-ha-haproxy:6379
*/}}
{{- define "kevent-gateway.redisAddr" -}}
{{- printf "%s-redis-ha-haproxy:6379" .Release.Name }}
{{- end }}

{{/*
Nom de la ConfigMap contenant la configuration du gateway.
Utilise la ConfigMap existante si spécifiée, sinon celle créée par ce chart.
*/}}
{{- define "kevent-gateway.configMapName" -}}
{{- if .Values.config.existingConfigMap -}}
{{- .Values.config.existingConfigMap -}}
{{- else -}}
{{- include "kevent-gateway.fullname" . -}}
{{- end -}}
{{- end }}

{{/*
Nom du Secret contenant les credentials S3.
Utilise le Secret existant si spécifié, sinon celui créé par ce chart.
*/}}
{{- define "kevent-gateway.s3SecretName" -}}
{{- if .Values.s3.existingSecret -}}
{{- .Values.s3.existingSecret -}}
{{- else -}}
{{- printf "%s-s3" (include "kevent-gateway.fullname" .) -}}
{{- end -}}
{{- end }}

{{/*
Nom du Secret contenant le mot de passe SASL Kafka.
Utilise le Secret existant si spécifié, sinon celui créé par ce chart.
*/}}
{{- define "kevent-gateway.kafkaSecretName" -}}
{{- if .Values.kafka.sasl.existingSecret -}}
{{- .Values.kafka.sasl.existingSecret -}}
{{- else -}}
{{- printf "%s-kafka" (include "kevent-gateway.fullname" .) -}}
{{- end -}}
{{- end }}

{{/*
Nom du Secret contenant la clé de chiffrement AES-256-GCM.
Utilise le Secret existant si spécifié, sinon celui créé par ce chart.
*/}}
{{- define "kevent-gateway.encryptionSecretName" -}}
{{- if .Values.encryption.existingSecret -}}
{{- .Values.encryption.existingSecret -}}
{{- else -}}
{{- printf "%s-encryption" (include "kevent-gateway.fullname" .) -}}
{{- end -}}
{{- end }}
