# aws-tool（Go 版）

一个 **Windows 可执行（exe）** 的 AWS 管理工具，基于 **Go + AWS SDK v2**，  
用于 **EC2 和 Lightsail（光帆）** 的创建与管理。

本工具特点：
- 运行后再输入 AWS 凭证（AK / SK），不写死、不落盘
- 支持 EC2 + Lightsail
- 支持创建 / 启动 / 停止 / 重启 / 删除实例
- 创建时可选：
  - 是否 **端口全开（0–65535 TCP/UDP）**
  - 是否添加 **启动初始脚本（user-data）**

---

## 🚀 功能概览

### EC2
- 创建 EC2 实例
- 可选：
  - 创建/使用安全组并 **全开端口**
  - 设置 **user-data 启动脚本**（自动 Base64）
- 管理 EC2：
  - 启动 / 停止 / 重启 / 终止
  - 自动扫描所有 Region 查找实例

### Lightsail（光帆）
- 创建 Lightsail 实例
- 可选：
  - **端口全开（TCP/UDP 0–65535）**
  - **user-data 启动脚本**
- 管理 Lightsail：
  - 启动 / 停止 / 重启
  - 自动扫描所有 Region


