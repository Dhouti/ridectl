apiVersion: summon.ridecell.io/v1beta1
kind: SummonPlatform
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
spec:
  version: fill_in_version
---
apiVersion: secrets.ridecell.io/v1beta1
kind: EncryptedSecret
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
data: {}
