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
