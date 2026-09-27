# Home 证书注册的 TLS 兼容要求

## 服务端升级要求

Home JWT 注册不再通过明文 TCP 发送 `enrollment_secret`，也不会在 TLS 失败后
回退到明文。外部 Home 服务需要先满足以下要求，再部署使用此注册流程的客户端：

- JWT 的 `ip:port` 必须直接接受 TLS 1.2 或更高版本。首次注册时客户端尚无
  客户端证书，因此服务端须允许在没有客户端证书的 TLS 连接上执行
  `CERTIFICATE REQUEST`，并在应用层验证注册密钥。
- TLS 握手必须发送服务端叶证书、必要的中间 CA，以及 JWT `ca_fingerprint`
  对应的 CA 证书；使用自签名根 CA 时也必须发送该根证书。仅配置本机信任库或
  在注册响应中返回 CA，不能代替握手中的 CA。
- `ca_fingerprint` 是对应 CA 证书 DER 内容的 SHA-256 指纹；允许大小写、冒号及
  空格分隔。该证书必须具备 CA 和签发证书的约束。
- 客户端仅以指纹匹配的 CA 为此次注册的信任锚，并完整验证服务端证书链、有效期、
  `serverAuth` 用途和目标名称。JWT 的 `ip` 为 IP 时，服务端证书必须包含匹配的
  IP SAN；为域名时，必须包含匹配的 DNS SAN。仅将可信 CA 附加到不相关的证书链，
  或只设置叶证书的 Common Name，不足以通过认证。

客户端完成上述 TLS 握手后，才在加密连接内发送原有 RESP 请求：

```text
CERTIFICATE REQUEST <certificate_id> <enrollment_secret> <CSR PEM>
```

响应仍为包含 JSON 的 RESP bulk string，JSON 字段保持 `ok`、`certificate`、`ca`。
`certificate` 的首张证书必须与 CSR 私钥匹配，Common Name 必须等于
`certificate_id`，并具备有效的客户端身份和可用于 `clientAuth` 的证书链；必要的
中间证书放在其后。`ca` 只能包含指纹匹配的单张 CA 证书，不能夹带其他信任根。

TLS 的自定义验证仅用于从指纹建立初始信任，并非跳过证书认证。JWT 必须经可信渠道
提供；本地流程解析 JWT 字段，不负责验证其签名，不能把来源不明的 JWT 当作可信
服务端配置。注册连接的 30 秒预算仅覆盖凭据获取，后续 Home 连接不新增超时。

## 既有缓存与身份切换

当前保留原有固定路径：

```text
~/.cli-proxy-api/client-crt.pem
~/.cli-proxy-api/client-key.pem
~/.cli-proxy-api/home-ca-crt.pem
```

缓存复用前会验证 CA 指纹、叶证书与私钥匹配、有效期、完整证书链、`clientAuth`
用途以及叶证书 Common Name 与当前 `certificate_id` 一致。即使 CA 相同，也不会
复用其他节点的客户端证书。新签发的证书在写入缓存前执行相同校验。

遇到身份不符、过期、错误用途、损坏或缺失 CA 的完整缓存时，客户端明确报错，
不会静默覆盖既有身份，也不会发送注册密钥尝试修补。需要重新注册时，请先停止使用
这些文件的实例，将上述三个文件作为同一组安全备份并移出缓存位置，再使用新 JWT
执行注册。不要只替换其中一个文件，也不要将私钥复制到日志或工单中。

此修复未增加多身份缓存命名空间或跨进程写锁。不同节点并行运行时应使用隔离的
操作系统用户/用户目录，避免共用上述三个路径。新建目录使用 `0700`，凭据文件
权限保持 `0600`；已有用户目录的权限由部署方检查。
