# Documentação das Correções Críticas: TJMG (PJe) e TRT 3 (CAdES)

## Agradecimento Especial e Contexto
Gostaria de registrar que este repositório (`pje_headless`) foi **absolutamente imprescindível** para o meu trabalho. Eu desenvolvi um driver open-source do zero para o token da StarSign ([starsign-driver](https://github.com/DiegoRibeirodeSouza/starsign-driver)), mas me deparei com um bloqueio grave: o aplicativo oficial em Java do PJeOffice simplesmente se recusava a reconhecer o token com o meu driver (apesar de conseguir encontrar o caminho do `.so`). 

Sem o `pje_headless` para contornar o PJeOffice oficial, o meu driver open-source seria praticamente inutilizado no sistema legado dos tribunais. Meu muito obrigado ao autor original pela fundação espetacular deste projeto!

---

## 1. O Problema do TJMG (PJe Legado e Endpoints `.seam`)
O `pje_headless` funcionava perfeitamente para sistemas modernos como o e-Proc, e era capaz de realizar o login sem problemas. Contudo, na hora de **assinar documentos e lotes** no PJe do TJMG, a requisição retornava erros HTTP 200, mas com falhas na aplicação:
1. `"Erro:A assinatura do arquivo não foi fornecida!"`
2. `"Erro:O hash do arquivo assinado não foi fornecido!"`

### Causa Raiz e Solução (`internal/pjeoffice/server.go`)
O `pje_headless` original estruturava o lote de respostas sempre como `application/json`. 
- **O Bug:** Os endpoints antigos de upload do PJe terminados em `.seam` são baseados no framework Java Server Faces (JSF). Eles **não suportam parseamento de JSON nativo** no corpo da requisição e exigem `application/x-www-form-urlencoded`.
- **A Solução:** Adicionamos um interceptador. Se a URL de destino (`target`) contiver `.seam`, ativamos o modo legado: convertemos o pacote JSON para formulário HTTP padrão e injetamos forçadamente os metadados críticos perdidos (`id`, `codIni`, `hash`, `isBin`) de volta no corpo do POST.

---

## 2. O Problema do TRT 3 (PJe KZ, CAdES e Hashing de Hardware)
Mesmo com o PJe funcionando, as assinaturas do TRT 3 (e assinaturas em lote CAdES) ainda falhavam. O token disparava um erro interno (`Assinatura Inválida` / `ASN-011`) ao tentar assinar grandes volumes.

### Causa Raiz e Solução (`internal/signer/pkcs11.go` e afins)
O TRT 3 trafega as assinaturas via pacotes enormes em Base64, usando o formato CAdES (PKCS#7).
- **O Bug:** O código original utilizava a instrução `CKM_SHA256_RSA_PKCS`, que obriga o chip (hardware) do SmartCard a realizar o hash criptográfico SHA-256 do documento internamente. Tokens como o StarSign possuem um buffer de memória interno muito pequeno. Quando o PJe enviava o buffer massivo (DER) do CAdES, o token travava por falta de capacidade de processamento.
- **A Solução:** 
  1. Implementamos suporte nativo ao endpoint `/base64` do PJe (envio de pacotes de assinatura via `conteudoBase64`).
  2. Alteramos a mecânica do `SHA256withRSA` para usar hashing manual (no lado do Host/Go). Rebaixamos a chamada PKCS#11 para o formato RAW (`CKM_RSA_PKCS`). Agora, o Go calcula o hash SHA-256 do arquivo na memória RAM do computador e concatena o prefixo estrutural ASN.1/DER (`DigestInfo` de 51 bytes). O token recebe apenas o bloco final, minúsculo, pronto para apenas criptografar com a chave privada, contornando o limite de buffer do hardware. 
  3. Adicionamos suporte de decodificação automática ao formato legado `ASN1MD5withRSA`.

---

## Contribuição Upstream (Pull Request #1)
No espírito do software livre, as correções para **ambos os problemas** (TJMG e TRT 3) foram empacotadas juntas e enviadas de volta ao criador original (`MrSchrodingers`) através de um Pull Request no repositório oficial (`MrSchrodingers/pje_headless#1`). Dessa forma, garantimos que outros desenvolvedores e usuários do sistema judiciário que enfrentem problemas com o JSF (`.seam`) ou com travamentos de hardware no TRT possam se beneficiar nativamente das soluções.
