{{- define "operator.name" -}}
{{- printf "%s-operator" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- define "operator.labels" -}}
app.kubernetes.io/name: {{ .Values.selectorName }}
app.kubernetes.io/instance: {{ .Release.Name | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service | quote }}
{{- end -}}
