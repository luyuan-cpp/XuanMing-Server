# 打包客户端的 dev TLS 信任(跨平台、正常校验、零设备配置)

> 结论:**打包时**(非 Shipping)把「引擎公网 CA 包 + dev CA」合并成
> `Pandora/Content/Certificates/cacert.pem` 让引擎 cook 进 pak,打完即删。
> 这是**跨平台、正常校验、零设备配置**的正解,Windows/Android/iOS 通用。
> dev-only 靠「只在非 Shipping 打包时注入这个文件」实现;Shipping 不注入 →
> 玩家用引擎公网 bundle,无 dev CA,无 MITM 风险。
>
> 定案:2026-07-06。实现:客户端仓库 `Tool/Build/Package.bat`。

## 解决什么问题

把 Development 包分发给**其它测试机**后,双击 `Pandora.exe` 登录报
`libcurl error 60 (SSL certificate problem: unable to get local issuer certificate)`。

根因:Windows 版 UE 客户端的 TLS 信任除了引擎内置 CA 包,还读 **Windows 系统根证书存储区**
(`WindowsPlatformSslCertificateManager` → `CertOpenSystemStoreW("ROOT")`)。
出包机跑过 `mkcert -install`(dev CA 进了 CurrentUser\Root)所以能连;
其它测试机没有这张 CA → 校验失败。而 `[SSL] DebuggingCertificatePath` 在打包版里
是相对路径、解析不可靠,指望不上。

## 被否决的方案(为什么不这么做)

| 方案 | 否决原因 |
| --- | --- |
| 每台测试机手动导入 CA(certutil / mkcert -install) | 多一步、不傻瓜,新人必踩坑 |
| 启动器 bat 先静默导入 CA 再启动游戏 | 测试同学是直接双击 Pandora.exe 的,不会走启动器 |
| 关闭 TLS 校验(`n.VerifyPeer=false` / skip verify) | dev 与生产行为不一致,证书问题拖到上线才暴露 |
| 把 dev CA 常驻提交到 `Content/Certificates/cacert.pem` | 会打进 **Shipping** 发行包,玩家设备信任 dev CA = MITM 风险 |
| 改引擎源码加信任逻辑 | 用的是 Installed Build,无法改;也没必要 |

## 机制(全部是引擎原生行为,不改引擎)

两条引擎权威规则(UE 5.8,已对照源码确认):

1. **打包 stage 规则** `Engine/Source/Programs/AutomationTool/Scripts/CopyBuildToStagingDirectory.Automation.cs`(~L2181):
   若存在 `<Project>/Content/Certificates/cacert.pem`,UAT 把它 stage 进 pak(UFS);
   否则回退引擎公网包 `Engine/Content/Certificates/ThirdParty/cacert.pem`。**所有平台一致。**
   ⚠️ **必要前提**:项目 `DefaultEngine.ini` 必须**显式**写
   `[/Script/Engine.NetworkSettings] n.VerifyPeer=true`。
   UAT 用 C# `GetBool(..., out bStageSSLCertificates)` 读这个 key,**key 缺失时 out 参数被重置为 false**
   (引擎源码自带注释:"GetBool will set Value to false if it's not found"),
   整个证书 stage 块被跳过,连引擎公网 cacert.pem 都不进包(2026-07-06 实测踩坑)。
2. **运行时加载规则** `SslCertificateManager.cpp::BuildRootCertificateArray`:
   优先加载 `ProjectContentDir/Certificates/cacert.pem` 作为可信根集,
   走正常 `CURLOPT_SSL_VERIFYPEER=1` 校验。**Windows/Android/iOS 同一路径同一逻辑。**

客户端仓库 `Tool/Build/Package.bat` 利用以上规则:

```
仅 Client 且 Config != Shipping:
  1. BuildCookRun 之前:
     copy  <引擎>/Engine/Content/Certificates/ThirdParty/cacert.pem   ← 公网 CA 包(约 225KB)
       →  Pandora/Content/Certificates/cacert.pem
     type  Pandora/Config/Certificates/pandora-dev-rootCA.pem  >>  追加合并 dev CA
  2. BuildCookRun(合并包被 cook 进 pak)
  3. 之后无论成败:del 该文件 + rd 目录(工作树打完即净,永不入库)
```

要点:
- **合并**而不是替换:公网 CA 信任全保留,只是多信任一张 dev CA。
- **正常校验**:不动 `n.VerifyPeer`,不跳过认证,与生产同一条校验链路。
- **零设备配置**:CA 在 pak 里,测试机双击 `Pandora.exe` 直连 Envoy :8443,
  不装证书、不改系统、不需要启动器。
- **Shipping 安全**:Shipping 打包不注入 → UAT 回退引擎公网包,玩家不信任 dev CA。
  临时文件打完即删,也不存在误提交入库的通道。
- **移动端留口**:Android/iOS 将来打包时,同一文件自动 cook 进各自的 pak,
  运行时同一路径加载,**无需任何额外改动**。移动端同样走真校验(接近线上)。

## 各场景的信任来源(速查)

| 场景 | dev CA 来源 | 说明 |
| --- | --- | --- |
| 编辑器 / PIE | `[SSL] DebuggingCertificatePath=Config/Certificates/pandora-dev-rootCA.pem` | 相对路径在编辑器下解析正常;见 [deploy/dev-ca/README.md](../../deploy/dev-ca/README.md) |
| Development/Test/DebugGame 打包 | pak 内合并版 `Content/Certificates/cacert.pem` | 本文机制,Package.bat 自动注入 |
| Shipping 打包 | **无 dev CA** | 引擎公网包;生产 Envoy 必须用公网 CA 真证书(见 release-checklist) |

## 证书轮换 / 换服务器 IP

- dev CA 换新:更新 `deploy/dev-ca/pandora-dev-rootCA.pem` 与客户端仓库
  `Pandora/Config/Certificates/pandora-dev-rootCA.pem`(同一张),**重新打包**即可,测试机无感。
- Envoy 换机器/换 IP:服务端证书 SAN 必须含新 IP,用同一 dev CA 重签
  (`mkcert <ip> localhost ...`),客户端**不用**重打包(信任的是 CA,不是叶子证书)。

## 验证方法

1. 打包日志出现:`[Package] Dev TLS: merged engine CA bundle + dev CA -> ...`,
   结束后出现 `removed temporary cooked CA bundle`。
2. pak 内容含 `Pandora/Content/Certificates/cacert.pem`
   (`UnrealPak <pak> -List | findstr Certificates`)。
3. 干净测试机(未导入过任何 dev CA)双击 `Pandora.exe`,登录成功、无 error 60。
4. Shipping 包复查:pak 里**不得**出现项目侧 `Certificates/cacert.pem` 合并版。
