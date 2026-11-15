{{- define "stellwerk.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}{{ .Release.Name | trunc 63 | trimSuffix "-" }}{{- else -}}{{ printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" }}{{- end -}}
{{- end -}}

{{- define "stellwerk.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "stellwerk.selector" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "stellwerk.secretName" -}}
{{- if .Values.existingSecret -}}{{ .Values.existingSecret }}{{- else -}}{{ include "stellwerk.fullname" . }}{{- end -}}
{{- end -}}

{{/* The config as stellwerk sees it: the user's config plus what the chart decides. */}}
{{- define "stellwerk.config" -}}
{{- $c := deepCopy .Values.config -}}
{{- $_ := set $c "listen" ":8080" -}}
{{- $ui := dict "listen" (ternary ":8081" "" .Values.ui.enabled) -}}
{{- $_ := set $c "ui" (merge $ui (default (dict) $c.ui)) -}}
{{- if .Values.metrics.enabled -}}{{- $_ := set $c "metrics" (dict "listen" ":9090") -}}{{- end -}}
{{- $_ := set $c "log" (dict "format" .Values.log.format "level" .Values.log.level) -}}
{{- $delivery := default (dict) $c.delivery -}}
{{- $_ := set $delivery "spool" (default "/data/spool.db" $delivery.spool) -}}
{{- $_ := set $c "delivery" $delivery -}}
{{- $audit := default (dict) $c.audit -}}
{{- $_ := set $audit "file" (default "/data/audit.jsonl" $audit.file) -}}
{{- $_ := set $c "audit" $audit -}}
{{- range $name, $t := $c.targets -}}
{{- if eq $t.type "matrix" -}}
{{- $s := default (dict) $t.settings -}}
{{- $_ := set $s "database" (default "/data/{target}-{account}-{node}.db" $s.database) -}}
{{- $_ := set $t "settings" $s -}}
{{- end -}}
{{- end -}}
{{- if gt (int .Values.ha.replicas) 1 -}}
{{- $fn := include "stellwerk.fullname" . -}}
{{- $peers := list -}}
{{- range $i := until (int .Values.ha.replicas) -}}
{{- $peers = append $peers (dict "id" (printf "%s-%d" $fn $i) "url" (printf "http://%s-%d.%s-peers:8080" $fn $i $fn)) -}}
{{- end -}}
{{- $_ := set $c "ha" (dict "enabled" true "peers" $peers) -}}
{{- end -}}
{{- toYaml $c -}}
{{- end -}}
