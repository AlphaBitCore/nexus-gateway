{{/* Common labels + name helpers. */}}
{{- define "nexus.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "nexus.fullname" -}}
{{- printf "%s-%s" .Release.Name (include "nexus.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "nexus.labels" -}}
app.kubernetes.io/name: {{ include "nexus.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{/* Image reference for a service: <registry>/<repo>:<tag>. */}}
{{- define "nexus.image" -}}
{{- printf "%s/%s:%s" .root.Values.images.registry .repo (.root.Values.images.tag | toString) -}}
{{- end -}}

{{/* The Secret holding shared secrets — either the user's or ours. */}}
{{- define "nexus.secretName" -}}
{{- if .Values.secrets.existingSecret -}}
{{- .Values.secrets.existingSecret -}}
{{- else -}}
{{- printf "%s-secrets" (include "nexus.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/* DATABASE_URL: external override wins, else the in-cluster postgresql subchart. */}}
{{- define "nexus.databaseUrl" -}}
{{- if .Values.external.databaseUrl -}}
{{- .Values.external.databaseUrl -}}
{{- else -}}
{{- printf "postgresql://%s@%s-postgresql:5432/%s" .Values.postgresql.auth.username .Release.Name .Values.postgresql.auth.database -}}
{{- end -}}
{{- end -}}

{{- define "nexus.redisAddr" -}}
{{- if .Values.external.redisAddr -}}
{{- .Values.external.redisAddr -}}
{{- else -}}
{{- printf "%s-valkey-master:6379" .Release.Name -}}
{{- end -}}
{{- end -}}

{{- define "nexus.natsUrl" -}}
{{- if .Values.external.natsUrl -}}
{{- .Values.external.natsUrl -}}
{{- else -}}
{{- printf "nats://%s-nats:4222" .Release.Name -}}
{{- end -}}
{{- end -}}

{{/*
Resolve one shared-secret value to base64: explicit user value wins, else the
value already stored in the release's Secret (persist across upgrade), else a
fresh random. Arg: dict (given, existingB64, len).
*/}}
{{- define "nexus.secretVal" -}}
{{- if .given }}{{ .given | b64enc }}
{{- else if .existingB64 }}{{ .existingB64 }}
{{- else }}{{ randAlphaNum .len | b64enc }}{{- end }}
{{- end -}}

{{/* Shared env block every service needs: infra addrs + secret refs. */}}
{{- define "nexus.commonEnv" -}}
- name: DATABASE_URL
  value: {{ include "nexus.databaseUrl" . | quote }}
- name: NEXUS_REDIS_ADDR
  value: {{ include "nexus.redisAddr" . | quote }}
- name: NEXUS_NATS_URL
  value: {{ include "nexus.natsUrl" . | quote }}
- name: NEXUS_HUB_URL
  value: {{ printf "http://%s-hub:3060" (include "nexus.fullname" .) | quote }}
- name: INTERNAL_SERVICE_TOKEN
  valueFrom:
    secretKeyRef: {name: {{ include "nexus.secretName" . }}, key: internalServiceToken}
- name: CREDENTIAL_ENCRYPTION_KEY
  valueFrom:
    secretKeyRef: {name: {{ include "nexus.secretName" . }}, key: credentialEncryptionKey}
{{- end -}}
