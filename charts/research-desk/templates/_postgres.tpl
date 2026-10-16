{{- define "desk.postgresEnv" -}}
- name: RESEARCH_POSTGRES_HOST
  value: {{ .root.Values.postgres.host | quote }}
- name: RESEARCH_POSTGRES_DATABASE
  value: {{ .root.Values.postgres.database | quote }}
- name: RESEARCH_POSTGRES_USERNAME_FILE
  value: /postgres-auth/username
- name: RESEARCH_POSTGRES_PASSWORD_FILE
  value: /postgres-auth/password
- name: RESEARCH_POSTGRES_CA_FILE
  value: /postgres-ca/ca.crt
- name: RESEARCH_POSTGRES_WRITER
  value: {{ .writer | quote }}
{{- end -}}
{{- define "desk.postgresMounts" -}}
- name: postgres-auth
  mountPath: /postgres-auth
  readOnly: true
- name: postgres-ca
  mountPath: /postgres-ca
  readOnly: true
{{- end -}}
{{- define "desk.postgresVolumes" -}}
- name: postgres-auth
  secret:
    secretName: {{ .Values.postgres.secretName }}
    items:
      - key: {{ .Values.postgres.usernameKey }}
        path: username
      - key: {{ .Values.postgres.passwordKey }}
        path: password
- name: postgres-ca
  secret:
    secretName: {{ .Values.postgres.caSecretName }}
    items:
      - key: {{ .Values.postgres.caKey }}
        path: ca.crt
{{- end -}}
