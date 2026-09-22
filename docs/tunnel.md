# Cloudflare 隧道（cloudflared）对接

把公网域名通过 Cloudflare 隧道转发到本机 SRCOS 的完整指南。

核心结论：SRCOS 的原生 TLS 与 cloudflared 的严格证书校验如何配合，以及常见的两个坑。

## 场景与架构

```
公网用户
  ↓ https://yuan.example.com        （Cloudflare 边缘，正规 TLS，自动证书）
  ↓ Cloudflare 隧道（cloudflared，QUIC 回连）
  ↓ 本机回环 https://localhost:7658  ← 这一步要过 cloudflared 的证书校验
SRCOS（原生 TLS）
```

cloudflared 的 ingress 规则（云端配置或本地 `config.yml`）：

```yaml
- hostname: yuan.example.com
  service: https://localhost:7658
  originRequest: {}
```

其他端口「照抄就能通」是因为那些是 HTTP 服务（`service: http://localhost:xxxx`），不涉及 TLS 校验；
SRCOS 一旦开 HTTPS，就必须处理证书验证这一环。

## 坑 1：自签证书必然被拒

cloudflared 连接 origin 时默认严格校验 TLS 证书（系统信任库 + hostname 匹配）。
`srcos serve --tls-selfsigned` 生成的自签证书没有可信根，握手直接失败：

```
ERR Unable to reach the origin service: tls: failed to verify certificate:
x509: certificate signed by unknown authority
```

症状就是：隧道域名打不开，`journalctl -u cloudflared` 里刷上面这条错误。这不是延时生效的问题，是证书信任问题。

先试的「快速开关」`noTLSVerify` 在面板 UI 里通常找不到 —— 它是 YAML 级配置
（ingress 规则里加 `noTLSVerify: true`），Cloudflare Zero Trust 面板的 Public Hostname 表单没有这个选项。

## 坑 2：Origin CA 方案的 hostname 不匹配

「去 Cloudflare 签一张 Origin CA 证书」听起来正规，但有一个隐藏的硬伤：
cloudflared 连接 `https://localhost:7658` 时，会用 URL 的主机名 `localhost` 去验证证书 SAN，
而 Origin CA 面板只能签公网域名（UI 拒绝单标签主机名）。两者必然不匹配：

```
SSL: no alternative certificate subject name matches target hostname 'localhost'
```

绕开它需要在面板里额外配置 `originServerName: <域名>`（同样是不容易找到的高级选项）。

结论：Origin CA 只适合「cloudflared 直连域名 origin」的场景，不适合「直连 localhost」。

## 正解：本地 CA + localhost SAN + 系统信任库

自己建一个本地 CA，签发一张 SAN 覆盖 `localhost`（以及局域网 IP、主机名）的服务器证书，
把 CA 装进本机系统信任库。cloudflared 用系统库验证 → 信任 ✓，SAN 含 localhost → hostname 匹配 ✓，
面板零改动。附带的好处：局域网内直接访问 `https://192.168.0.106:7658` 的浏览器
（读系统信任库）也不再有警告。

### ① 生成本地 CA 与服务器证书

```bash
mkdir -p /opt/srcos/config/tls/ca && cd /opt/srcos/config/tls/ca

# 本地 CA（20 年）
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
  -keyout ca.key -out ca.pem -days 7300 -nodes -subj "/CN=srcos-local-ca/O=srcos"
chmod 600 ca.key

# 服务器证书（SAN 必须含 localhost；局域网 IP/主机名按需补充）
openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
  -keyout server.key -out server.csr -nodes -subj "/CN=localhost/O=srcos"
cat > san.cnf << 'EOF'
subjectAltName=DNS:localhost,DNS:node01,IP:127.0.0.1,IP:0:0:0:0:0:0:0:1,IP:192.168.0.106
EOF
openssl x509 -req -in server.csr -CA ca.pem -CAkey ca.key -CAcreateserial \
  -out server.pem -days 3650 -extfile san.cnf
chmod 600 server.key

# 自检：证书链合法 + SAN 齐全
openssl verify -CAfile ca.pem server.pem
```

### ② 安装 CA 到系统信任库（cloudflared 用系统库）

```bash
sudo cp /opt/srcos/config/tls/ca/ca.pem /usr/local/share/ca-certificates/srcos-local-ca.crt
sudo update-ca-certificates
# 输出应包含 Adding debian:srcos-local-ca.pem ... done
```

### ③ 让 SRCOS 使用新证书

```bash
# 方式一：启动参数
./srcos serve --port 7658 --tls-cert /opt/srcos/config/tls/ca/server.pem \
  --tls-key /opt/srcos/config/tls/ca/server.key
```

```yaml
# 方式二：写入 state.yaml 后直接 ./srcos serve（TLS 配置会持久化）
server:
  tls_cert: /opt/srcos/config/tls/ca/server.pem
  tls_key:  /opt/srcos/config/tls/ca/server.key
```

### ④ 重启 cloudflared（关键一步，容易漏）

cloudflared 是 Go 写的，系统根证书池在进程启动时加载并缓存。它可能已经连续运行数周 ——
即使 CA 已装进系统库，它仍用缓存里的旧根池校验，继续报 `certificate signed by unknown authority`。
必须重启 cloudflared：

```bash
sudo systemctl restart cloudflared
```

### ⑤ 验证

```bash
# 本机严格验证（模拟 cloudflared 的校验方式，读系统信任库）
curl -s -o /dev/null -w "%{http_code}\n" https://localhost:7658/    # 200
curl -s -o /dev/null -w "%{http_code}\n" https://yuan.example.com/  # 200，走完整公网链路

# 观察日志里不再出现证书错误
journalctl -u cloudflared --no-pager | grep -i "unknown authority" | tail   # 空
```

## 其他设备信任本机 CA（可选）

局域网其他设备直接访问 `https://192.168.0.106:7658` 时仍会提示不受信任（CA 只装在这台服务器上）。
让它们免警告，把 `ca.pem` 拷过去导入系统/浏览器信任库：

- Linux：`cp ca.pem /usr/local/share/ca-certificates/ && sudo update-ca-certificates`
- Windows：双击导入「受信任的根证书颁发机构」
- macOS：钥匙串访问导入并设为始终信任

不导入也能用，只是每次访问要点一次「继续」。
