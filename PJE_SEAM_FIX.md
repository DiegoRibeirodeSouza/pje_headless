# Documentação da Correção: Assinatura de Lotes em Endpoints .seam (PJe)

## Agradecimento Especial e Contexto
Gostaria de registrar que este repositório (`pje_headless`) foi **absolutamente imprescindível** para o meu trabalho. Eu desenvolvi um driver open-source do zero para o token da StarSign ([starsign-driver](https://github.com/DiegoRibeirodeSouza/starsign-driver)), mas me deparei com um bloqueio grave: o aplicativo oficial em Java do PJeOffice simplesmente se recusava a reconhecer o token com o meu driver (apesar de conseguir encontrar o caminho do `.so`).

Sem o `pje_headless` para contornar o PJeOffice oficial, o meu driver open-source seria praticamente inutilizado no sistema legado do PJe do TJMG. Meu muito obrigado ao autor original pela fundação espetacular deste projeto!

---
## O Problema Original
O `pje_headless` funciona perfeitamente para sistemas modernos como o e-Proc, e era capaz de realizar o **login** no PJe sem problemas. Contudo, na hora de **assinar documentos e lotes** no PJe (ex: PJe do TJMG), a requisição retornava erros HTTP 200, mas com mensagens de falha na aplicação:

1. `"Erro:A assinatura do arquivo não foi fornecida!"`
2. `"Erro:O hash do arquivo assinado não foi fornecido!"`

## Causa Raiz
A investigação revelou que o problema residia no formato de empacotamento da requisição HTTP (HTTP POST Body) feita do `pje_headless` para o servidor do tribunal:

- O código original do `pje_headless` estruturava o lote de respostas (batch mode) como uma matriz (array) JSON (ex: `application/json`), contendo chaves como `id`, `assinatura` e `cadeiaCertificado`.
- **A incompatibilidade:** Os endpoints antigos de upload do PJe terminados em `.seam` (como `/arquivoAssinadoUpload.seam`) são baseados no framework Java Server Faces (JSF). Eles **não suportam parseamento de JSON nativo** no corpo da requisição. Eles exigem dados no formato tradicional `application/x-www-form-urlencoded`.
- Por não conseguir ler o JSON, o framework Java interpretava que todas as variáveis (`assinatura`, `hash`, etc) tinham vindo vazias ou nulas.
- Além disso, o PJe exige os metadados estruturais originais (como `hash` e `codIni`) de volta no POST final, e a implementação original enviava apenas a assinatura e os certificados.

## A Solução (Nossas Modificações)
Para corrigir este comportamento sem quebrar a compatibilidade com sistemas mais modernos (que aceitam JSON), implementamos as seguintes lógicas em `internal/pjeoffice/server.go`:

1. **Interceptação por Sufixo:**
   Adicionamos uma condicional inspecionando a URL de destino (`target`). Se a string contiver `.seam`, ativamos o modo legado de compatibilidade.

2. **Formatação `application/x-www-form-urlencoded`:**
   Convertemos o array de arquivos processados para instâncias do tipo `url.Values`.

3. **Injeção de Metadados Críticos:**
   Enxertamos no corpo do formulário as variáveis originais que vinham ocultas no pacote JSON de instrução (`t.Arquivos[i]`):
   - `id`
   - `codIni`
   - `hash`
   - `isBin`
   - E obviamente as respostas geradas `assinatura` e `cadeiaCertificado`.

Essas modificações resolveram inteiramente o problema de assinatura em lote, permitindo o funcionamento 100% autônomo do `pje_headless` no PJe, sem perda de sessão.

---

## Contribuição Upstream (Pull Request)
No espírito do software livre, essa correção foi empacotada e enviada de volta ao criador original (`MrSchrodingers`) através de um Pull Request no repositório oficial (`MrSchrodingers/pje_headless#1`), garantindo que outros desenvolvedores e usuários do sistema judiciário que enfrentem problemas com endpoints `.seam` possam se beneficiar nativamente da solução. Agradecemos novamente ao autor original pela arquitetura incrível que tornou essa ponte possível!
