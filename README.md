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

---

## 🖥️ 运行环境要求（Windows 11）

### ✅ 必须安装的软件

#### 1️⃣ Go（必须）
- 版本建议：**Go 1.21+**
- 官方下载地址：  
  👉 https://go.dev/dl/

安装完成后，验证：
```powershell
go version
2️⃣ Git（推荐，用于获取/更新代码）
官方下载地址：
👉 https://git-scm.com/downloads

Windows 直接下载：
👉 https://github.com/git-for-windows/git/releases/latest

安装完成后，验证：

powershell
复制代码
git --version
3️⃣ AWS 账号 + Access Key
需要你自己的 AWS 账号，并创建 Access Key：

Access Key ID

Secret Access Key
-（可选）Session Token（临时凭证）

本工具 不会保存你的凭证，只在运行时使用。

📦 获取代码
方式一：使用 Git（推荐）
powershell
复制代码
git clone https://github.com/yzhpxd/aws-tool.git
cd aws-tool
方式二：网页下载
打开仓库页面

点击 Code → Download ZIP

解压后进入目录

🔨 编译（生成 exe）
在项目目录执行：

powershell
复制代码
go build -o awsman.exe
生成文件：

复制代码
awsman.exe
▶️ 运行
powershell
复制代码
.\awsman.exe
运行后你需要输入：
AWS Access Key ID

AWS Secret Access Key
-（可选）AWS Session Token

引导 Region（用于拉取区域列表）

🔐 安全说明（重要）
❌ 不要把 AK / SK 写进代码

❌ 不要提交 awsman.exe 到 GitHub

❌ 不要提交 .aws/credentials

本项目已使用 .gitignore 忽略敏感文件

⚠️ 端口全开风险提示
如果选择 “端口全开（0–65535）”，实例将暴露在公网：

仅用于测试 / 临时用途

用完请及时关闭或删除实例

📄 License
MIT License（你可以自由修改、使用、二次开发）

📌 免责声明
本工具仅为学习和运维辅助用途，
使用过程中产生的费用、安全风险由使用者自行承担。

yaml
复制代码

---

## ✅ 下一步你可以做的事

在 `C:\limanage`（或仓库目录）：

```powershell
notepad README.md
粘贴上面的内容 → 保存 → 然后：

powershell
复制代码
git add README.md
git commit -m "add README"
git push
