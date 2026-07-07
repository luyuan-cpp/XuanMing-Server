# Pandora 本地开发 CA(dev TLS 证书认证)

本目录放**本地开发期的公开 CA 证书** `pandora-dev-rootCA.pem`,用于让开发者的 UE 客户端
**用真证书认证**(`bDevInsecureTls=false`)连本地 / 共享 dev 的 Envoy(:8443),
而不是关闭 TLS 校验。

## 这是什么 / 不是什么

- ✅ `pandora-dev-rootCA.pem`:**公开 CA 证书**(`-----BEGIN CERTIFICATE-----`),由 mkcert 生成,
  签发了 Envoy 的服务端证书。导入它 = 让 UE 的 OpenSSL 信任本地 Envoy 证书链。**不含私钥,入库安全。**
- ❌ **私钥**(`rootCA-key.pem` / `*.key`)留在签发机,**绝不入库**(AGENTS.md §3 红线)。

> ⚠️ 仅限**开发期**。生产环境 Envoy 用公网 CA(Let's Encrypt / 商业)签的真实域名证书,
> 玩家设备零配置即可校验,**不需要也不应该**导入这个 dev CA。见 [release-checklist.md](../../docs/ops/release-checklist.md) §3。

> 📦 本页机制(`DebuggingCertificatePath`)覆盖**编辑器 / PIE**。**打包客户端**(分发给其它测试机)
> 走另一套零配置机制:打包时合并 CA 进 pak,见
> [client-packaged-dev-tls.md](../../docs/ops/client-packaged-dev-tls.md)。

## 开发者怎么用(一次性)

新开发者克隆仓库后,跑一次导入脚本:

```powershell
pwsh tools/scripts/import_dev_ca.ps1
```

脚本会(幂等、**不碰共享引擎目录**):
1. 把本目录公开 CA 复制到 **UE 客户端工程** `<UE工程路径>\Config\Certificates\pandora-dev-rootCA.pem`;
2. 确保客户端 `Config/DefaultEngine.ini` 有 `[SSL] DebuggingCertificatePath` 指向它;
3. 若历史上往引擎 `cacert.pem` 追加过 dev CA,从备份还原(撤销污染)。

机制(已对照引擎源码 `SslCertificateManager.cpp::BuildRootCertificateArray` 确认):
UE 的 `[SSL] DebuggingCertificatePath` 把**一张额外证书叠加到**引擎公网 CA 包**之上**
(不替换,公网 CA 信任全保留),且**仅在非 Shipping**(编辑器 / Development)生效。
证书放工程 `Config/`(不在 `Content`)→ **绝不打进发行包**,玩家拿不到。

导入后,UE 用 `bDevInsecureTls=false` 连本地 Envoy 也能通过 TLS 校验,
和生产同一条校验链路。**为什么不直接关校验**:关校验(`bDevInsecureTls=true`)会让 dev 与生产行为不一致,
容易把证书问题拖到上线才暴露;叠加 dev CA 后 dev 全程走真校验,最接近真实。

> **为什么不放引擎目录 / 不放工程 Content**:引擎目录是多项目共享、升级会被覆盖、又不在仓库里(队友拉不到);
> 工程 `Content/Certificates/cacert.pem` 是**整包替换**引擎公网 CA(会丢公网 CA 信任)且会**打进发行包给玩家**。
> `Config/` + `DebuggingCertificatePath` 三者皆免:不替换、不入包、不碰引擎。

## 证书轮换

如果签发机重新 `mkcert` 生成了新 CA(根证书变了),需要:
1. 用新的公开 CA 覆盖本目录 `pandora-dev-rootCA.pem`;
2. 通知开发者重跑 `import_dev_ca.ps1`(脚本幂等,会追加新 CA)。
