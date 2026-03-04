# appcoding-agent

一键安装：

```bash
curl -fsSL https://raw.githubusercontent.com/hongliangzhang07/appcoding-agent/main/install/install.sh | bash
```

离线安装（先把安装包下载到本地）：

```bash
APP_AGENT_ARCHIVE_FILE=/path/to/appcoding-agent_linux_amd64.tar.gz \
bash install/install.sh
```

离线完整包（单文件下载）：

```bash
# 下载 Release 里的 appcoding-agent_offline_bundle.tar.gz 后执行
tar -xzf appcoding-agent_offline_bundle.tar.gz
cd appcoding-agent_offline_bundle
bash install-offline.sh
```

启动检查：

```bash
appcoding-agentctl status
appcoding-agentctl health
appcoding-agentctl pairing
```

一键卸载：

```bash
curl -fsSL https://raw.githubusercontent.com/hongliangzhang07/appcoding-agent/main/install/uninstall.sh | bash
```
