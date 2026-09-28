{{- define "type" -}}
{{- $type := . -}}
{{- if markdownShouldRenderType $type -}}

#### {{ $type.Name }}

{{ if $type.IsAlias }}_Underlying type:_ _{{ markdownRenderTypeLink $type.UnderlyingType  }}_{{ end }}

{{ template "proseDoc" $type.Doc }}

{{ if $type.Validation -}}
_Validation:_
{{- range $type.Validation }}
- {{ template "proseDoc" (. | replace ": {}" "") }}
{{- end }}
{{- end }}

{{ $refs := list -}}
{{ range $type.SortedReferences -}}
{{ if markdownShouldRenderType . }}{{ $refs = append $refs . }}{{ end -}}
{{ end -}}
{{ if $refs -}}
_Appears in:_
{{- range $refs }}
- {{ markdownRenderTypeLink . }}
{{- end }}
{{- end }}

{{ if $type.Members -}}
| Field | Description | Default | Validation |
| --- | --- | --- | --- |
{{ if $type.GVK -}}
| `apiVersion` _string_ | `{{ $type.GVK.Group }}/{{ $type.GVK.Version }}` | | |
| `kind` _string_ | `{{ $type.GVK.Kind }}` | | |
{{ end -}}

{{ range $type.Members -}}
| `{{ .Name  }}` _{{ markdownRenderType .Type }}_ | {{ template "type_members" . }} | {{ markdownRenderDefault .Default }} | {{ range .Validation -}} {{ template "cellDoc" (. | replace ": {}" "") }} <br />{{ end }} |
{{ end -}}

{{ end -}}

{{ if $type.EnumValues -}} 
| Field | Description |
| --- | --- |
{{ range $type.EnumValues -}}
| `{{ .Name }}` | {{ template "cellDoc" .Doc }} |
{{ end -}}
{{ end -}}


{{- end -}}
{{- end -}}
