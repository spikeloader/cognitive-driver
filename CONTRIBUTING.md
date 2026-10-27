# Contribuindo

Obrigado pelo interesse! Issues e pull requests são bem-vindos, em português
ou inglês.

## Ambiente

Requisitos: Go (versão em `go.mod`), bash e, para testar a instalação, um
Linux com systemd.

```bash
make build      # bin/cognitive-driver
make test       # testes com -race
make vet fmt-check
make run        # roda local com configs/config.yaml (estado em ./data)
```

`scripts/validate-e2e.sh --local` faz a validação ponta a ponta sem instalar
(~6–10 min).

## Pull requests

- Um assunto por PR, com testes para comportamento novo ou corrigido.
- `make fmt-check vet test` precisa passar (a CI roda o mesmo, além de
  `shellcheck` nos scripts).
- Mudanças visíveis entram em `CHANGELOG.md`, na seção "Não lançado".
- Mantenha o estilo do código ao redor: comentários e mensagens em
  português, sem dependências novas sem necessidade.

## Novo driver de serviço

A maioria dos serviços não exige código: basta um YAML em `deploy/drivers/`
lendo o exporter do serviço (veja os existentes e a seção "Outros serviços"
do README). Inclua uma amostra real do exporter em
`internal/feature/testdata/` e um teste em `internal/feature/feature_test.go`
que carregue o YAML e confira as features.

## Princípio de segurança

O projeto é `observe_only`: drivers não executam ações no host. Contribuições
que executem comandos ou alterem o sistema precisam passar pelo pacote
`internal/policy` e ser discutidas em uma issue antes.

## Licença

Ao contribuir, você concorda que sua contribuição é licenciada sob a
[Apache License 2.0](LICENSE), conforme a seção 5 da licença.
