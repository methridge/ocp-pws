# Personal Weather Station Current Data

Modified version of my [CLI pws application](https://github.com/methridge/pws)
to run in [OpenShift](https://www.openshift.com/). This also uses
[HashiCorp Vault](https://www.vaultproject.io) with the
[Vault Secrets Operator](https://developer.hashicorp.com/vault/docs/deploy/kubernetes/vso)
to retrieve secrets.

Reads the current conditions from Wunderground.com for my PWS.
