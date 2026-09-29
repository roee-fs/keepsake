{{- /* The Secret holding app-dsn and owner-dsn: the operator's in existing mode when
       named, else the one secret.yaml renders. */}}
{{- define "keepsake.dsnSecret" -}}
{{- if and (eq .Values.postgres.mode "existing") .Values.postgres.existingSecret -}}
{{- .Values.postgres.existingSecret -}}
{{- else -}}
{{- printf "%s-dsn" .Release.Name -}}
{{- end -}}
{{- end }}
