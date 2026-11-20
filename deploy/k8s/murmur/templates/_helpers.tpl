{{/* Labels on every object, so `kubectl get all -l app.kubernetes.io/part-of=murmur` finds them. */}}
{{- define "murmur.labels" -}}
app.kubernetes.io/part-of: murmur
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end }}

{{/* The URL services connect to NATS with: every server, so a client can reconnect to another one. */}}
{{- define "murmur.natsURL" -}}
{{- $urls := list -}}
{{- range $i := until (int .Values.nats.replicas) -}}
{{- $urls = append $urls (printf "nats://nats-%d.nats-headless:4222" $i) -}}
{{- end -}}
{{ join "," $urls }}
{{- end }}

{{/* A Murmur service's image: murmur.image "ingester" . */}}
{{- define "murmur.image" -}}
{{- $root := index . 1 -}}
image: {{ printf "%s/%s:%s" $root.Values.image.repository (index . 0) $root.Values.image.tag }}
imagePullPolicy: {{ $root.Values.image.pullPolicy }}
{{- end }}

{{/* The flags every service that talks to NATS shares; the stream keeps one copy per NATS server. */}}
{{- define "murmur.natsArgs" -}}
- -nats={{ include "murmur.natsURL" . }}
- -stream-replicas={{ .Values.nats.replicas }}
{{- end }}

{{/* DATABASE_URL from the Secret, for args to use as $(DATABASE_URL). */}}
{{- define "murmur.dbEnv" -}}
- name: DATABASE_URL
  valueFrom:
    secretKeyRef: { name: murmur-db, key: DATABASE_URL }
{{- end }}

{{/* Ready once /metrics answers, as in Docker Compose. */}}
{{- define "murmur.metricsProbe" -}}
readinessProbe:
  httpGet: { path: /metrics, port: metrics }
  periodSeconds: 5
{{- end }}
