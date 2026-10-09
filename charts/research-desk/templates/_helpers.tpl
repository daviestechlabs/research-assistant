{{- define "desk.name" -}}
{{- if and .Values.legacyDeployment (ne .Release.Name "research-assistant") -}}
{{- fail "legacyDeployment is reserved for the research-assistant migration" -}}
{{- end -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- define "desk.labels" -}}
app.kubernetes.io/name: research-assistant
app.kubernetes.io/instance: {{ .Release.Name | quote }}
app.kubernetes.io/part-of: research-desk
app.kubernetes.io/managed-by: {{ .Release.Service | quote }}
{{- end -}}
{{- define "desk.selector" -}}
{{- if .Values.legacyDeployment }}
app: research-assistant
{{- else }}
app.kubernetes.io/name: research-assistant
app.kubernetes.io/instance: {{ .Release.Name | quote }}
{{- end }}
{{- end -}}
{{- define "desk.configName" -}}
{{- if .Values.legacyDeployment -}}research-config{{- else -}}{{ include "desk.name" . }}-config{{- end -}}
{{- end -}}
