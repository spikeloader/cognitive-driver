# Política de segurança

## Versões suportadas

Correções de segurança saem na versão mais recente publicada.

## Como reportar

**Não abra uma issue pública para vulnerabilidades.** Use o
[reporte privado de vulnerabilidades do GitHub](https://github.com/spikeloader/cognitive-driver/security/advisories/new)
com a descrição, os passos para reproduzir e o impacto esperado.

Você deve receber uma resposta inicial em até 7 dias. Depois da correção,
o aviso é publicado com crédito a quem reportou, se desejar.

## Escopo

Interessam especialmente: bypass da autenticação da API (`MICRO_AI_TOKEN`),
execução de comandos apesar do modo `observe_only`, injeção via métricas
coletadas ou YAML de driver, e escalonamento de privilégio pelos scripts de
instalação (`scripts/*.sh`, `cron-wrap`).
