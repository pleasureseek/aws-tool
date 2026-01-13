package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2t "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/lightsail"
	lst "github.com/aws/aws-sdk-go-v2/service/lightsail/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

/*
功能：
- 运行 exe 后输入 AK/SK
- 主菜单：
  1) EC2：建实例（可选全开端口 + 可选 user-data）
  2) EC2：控制实例（并发扫描所有 region）
  3) Lightsail：建光帆（优化版：默认 nano_3_0，数字选择套餐/系统）
  4) Lightsail：控制光帆（并发扫描所有 region；含静态IP管理）
*/

const bootstrapRegion = "us-east-1"

// --- 数据结构 ---

type LSInstanceRow struct {
	Idx    int
	Region string
	Name   string
	State  string
	IP     string
	AZ     string
}

type LSStaticIPRow struct {
	Idx        int
	Region     string
	Name       string
	IP         string
	AttachedTo string
	IsAttached bool
}

type EC2InstanceRow struct {
	Idx    int
	Region string
	AZ     string
	ID     string
	State  string
	Name   string
	Type   string
	PubIP  string
	PrivIP string
}

// -------------------- UI/辅助函数 --------------------

func regionCN(region string) string {
	m := map[string]string{
		"af-south-1":     "南非·开普敦",
		"ap-east-1":      "中国·香港",
		"ap-northeast-1": "日本·东京",
		"ap-northeast-2": "韩国·首尔",
		"ap-northeast-3": "日本·大阪",
		"ap-south-1":     "印度·孟买",
		"ap-south-2":     "印度·海得拉巴",
		"ap-southeast-1": "新加坡",
		"ap-southeast-2": "澳大利亚·悉尼",
		"ap-southeast-3": "印度尼西亚·雅加达",
		"ap-southeast-4": "澳大利亚·墨尔本",
		"ca-central-1":   "加拿大·中部",
		"eu-central-1":   "德国·法兰克福",
		"eu-central-2":   "瑞士·苏黎世",
		"eu-north-1":     "瑞典·斯德哥尔摩",
		"eu-south-1":     "意大利·米兰",
		"eu-south-2":     "西班牙·马德里",
		"eu-west-1":      "爱尔兰·都柏林",
		"eu-west-2":      "英国·伦敦",
		"eu-west-3":      "法国·巴黎",
		"il-central-1":   "以色列·特拉维夫",
		"me-central-1":   "阿联酋·阿布扎比",
		"me-south-1":     "巴林",
		"sa-east-1":      "巴西·圣保罗",
		"us-east-1":      "美国东部·弗吉尼亚",
		"us-east-2":      "美国东部·俄亥俄",
		"us-west-1":      "美国西部·加州(北)",
		"us-west-2":      "美国西部·俄勒冈",
	}
	if v, ok := m[region]; ok {
		return v
	}
	return "未知区域"
}

func input(prompt, def string) string {
	fmt.Print(prompt)
	r := bufio.NewReader(os.Stdin)
	s, _ := r.ReadString('\n')
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	return s
}

func inputSecret(prompt string) string {
	fmt.Print(prompt)
	r := bufio.NewReader(os.Stdin)
	s, _ := r.ReadString('\n')
	return strings.TrimSpace(s)
}

func mustInt(s string) int {
	i, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return -1
	}
	return i
}

func cut(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func yes(s string) bool {
	s = strings.TrimSpace(strings.ToLower(s))
	return s == "y" || s == "yes"
}

func collectUserData(promptTitle string) (raw string, isEmpty bool) {
	fmt.Println(promptTitle)
	fmt.Println("（直接回车跳过；如需输入多行，请输入内容后另起一行输入 END 结束）")
	var lines []string
	for {
		l := input("> ", "")
		if l == "" && len(lines) == 0 {
			return "", true
		}
		if l == "END" {
			break
		}
		lines = append(lines, l)
	}
	return strings.Join(lines, "\n"), len(lines) == 0
}

func mkCfg(ctx context.Context, region string, creds aws.CredentialsProvider) (aws.Config, error) {
	return config.LoadDefaultConfig(
		ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(creds),
	)
}

func stsCheck(ctx context.Context, region string, creds aws.CredentialsProvider) error {
	cfg, err := mkCfg(ctx, region, creds)
	if err != nil {
		return err
	}
	cli := sts.NewFromConfig(cfg)
	_, err = cli.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	return err
}

func pickFromList(title string, items []string, def string) (string, error) {
	if len(items) == 0 {
		return "", errors.New("列表为空")
	}
	fmt.Println(title)
	defIdx := 1
	for i := range items {
		if items[i] == def {
			defIdx = i + 1
			break
		}
	}
	for i, it := range items {
		fmt.Printf("  %2d) %-14s ------- %s\n", i+1, it, regionCN(it))
	}
	s := input(fmt.Sprintf("请输入编号 [%d]: ", defIdx), fmt.Sprintf("%d", defIdx))
	idx := mustInt(s)
	if idx < 1 || idx > len(items) {
		return "", fmt.Errorf("编号无效")
	}
	return items[idx-1], nil
}

// 打印对齐表格
func printTable(header string, rowsFunc func(*tabwriter.Writer)) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, header)
	rowsFunc(w)
	w.Flush()
}

// -------------------- Regions --------------------

func getEC2Regions(ctx context.Context, creds aws.CredentialsProvider) ([]string, error) {
	cfg, err := mkCfg(ctx, bootstrapRegion, creds)
	if err != nil {
		return nil, err
	}
	cli := ec2.NewFromConfig(cfg)
	out, err := cli.DescribeRegions(ctx, &ec2.DescribeRegionsInput{
		AllRegions: aws.Bool(true),
	})
	if err != nil {
		return nil, err
	}
	var rs []string
	for _, r := range out.Regions {
		if r.RegionName != nil && *r.RegionName != "" {
			rs = append(rs, *r.RegionName)
		}
	}
	sort.Strings(rs)
	return rs, nil
}

func getLightsailRegions(ctx context.Context, creds aws.CredentialsProvider) ([]string, error) {
	cfg, err := mkCfg(ctx, bootstrapRegion, creds)
	if err != nil {
		return nil, err
	}
	cli := lightsail.NewFromConfig(cfg)
	out, err := cli.GetRegions(ctx, &lightsail.GetRegionsInput{})
	if err != nil {
		return nil, err
	}
	var rs []string
	for _, r := range out.Regions {
		name := string(r.Name)
		if name != "" {
			rs = append(rs, name)
		}
	}
	sort.Strings(rs)
	return rs, nil
}

// -------------------- Lightsail 逻辑 --------------------

// lsListAll - 并发扫描
func lsListAll(ctx context.Context, regions []string, creds aws.CredentialsProvider) ([]LSInstanceRow, error) {
	var (
		mu   sync.Mutex
		rows = make([]LSInstanceRow, 0, 8)
		wg   sync.WaitGroup
	)

	fmt.Printf("正在并发扫描 %d 个 Lightsail 区域...\n", len(regions))

	for _, rg := range regions {
		wg.Add(1)
		go func(region string) {
			defer wg.Done()

			cfg, err := mkCfg(ctx, region, creds)
			if err != nil {
				return
			}
			cli := lightsail.NewFromConfig(cfg)
			out, err := cli.GetInstances(ctx, &lightsail.GetInstancesInput{})
			if err != nil {
				return
			}
			if len(out.Instances) == 0 {
				return
			}

			var localRows []LSInstanceRow
			for _, ins := range out.Instances {
				ip := ""
				if ins.PublicIpAddress != nil && *ins.PublicIpAddress != "None" {
					ip = *ins.PublicIpAddress
				}
				state := ""
				if ins.State != nil {
					state = aws.ToString(ins.State.Name)
				}
				az := ""
				if ins.Location != nil {
					az = aws.ToString(ins.Location.AvailabilityZone)
				}
				localRows = append(localRows, LSInstanceRow{
					Region: region,
					Name:   aws.ToString(ins.Name),
					State:  state,
					IP:     ip,
					AZ:     az,
				})
			}

			mu.Lock()
			rows = append(rows, localRows...)
			mu.Unlock()

		}(rg)
	}

	wg.Wait()

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Region == rows[j].Region {
			return rows[i].Name < rows[j].Name
		}
		return rows[i].Region < rows[j].Region
	})

	for i := range rows {
		rows[i].Idx = i + 1
	}

	return rows, nil
}

func lsWaitRunning(ctx context.Context, cli *lightsail.Client, name string, maxWait time.Duration) error {
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		o, err := cli.GetInstance(ctx, &lightsail.GetInstanceInput{InstanceName: &name})
		if err == nil && o.Instance != nil && o.Instance.State != nil && aws.ToString(o.Instance.State.Name) == "running" {
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("等待 running 状态超时")
}

func lsOpenAllPortsWithRetry(ctx context.Context, cli *lightsail.Client, name string) error {
	for i := 1; i <= 20; i++ {
		_, err := cli.PutInstancePublicPorts(ctx, &lightsail.PutInstancePublicPortsInput{
			InstanceName: aws.String(name),
			PortInfos: []lst.PortInfo{
				{FromPort: 0, ToPort: 65535, Protocol: lst.NetworkProtocolTcp},
				{FromPort: 0, ToPort: 65535, Protocol: lst.NetworkProtocolUdp},
			},
		})
		if err == nil {
			return nil
		}
		time.Sleep(6 * time.Second)
		if i == 20 {
			return err
		}
	}
	return fmt.Errorf("unknown")
}

func lsListStaticIPsInRegion(ctx context.Context, region string, creds aws.CredentialsProvider) ([]LSStaticIPRow, error) {
	cfg, err := mkCfg(ctx, region, creds)
	if err != nil {
		return nil, err
	}
	cli := lightsail.NewFromConfig(cfg)

	out, err := cli.GetStaticIps(ctx, &lightsail.GetStaticIpsInput{})
	if err != nil {
		return nil, err
	}

	rows := make([]LSStaticIPRow, 0, len(out.StaticIps))
	idx := 0
	for _, s := range out.StaticIps {
		idx++
		ip := aws.ToString(s.IpAddress)
		name := aws.ToString(s.Name)

		attached := ""
		isAttached := false
		if s.AttachedTo != nil && *s.AttachedTo != "" {
			attached = *s.AttachedTo
			isAttached = true
		}

		rows = append(rows, LSStaticIPRow{
			Idx:        idx,
			Region:     region,
			Name:       name,
			IP:         ip,
			AttachedTo: attached,
			IsAttached: isAttached,
		})
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	for i := range rows {
		rows[i].Idx = i + 1
	}
	return rows, nil
}

func lsFindStaticIPsAttachedTo(ctx context.Context, region, instanceName string, creds aws.CredentialsProvider) ([]LSStaticIPRow, error) {
	rows, err := lsListStaticIPsInRegion(ctx, region, creds)
	if err != nil {
		return nil, err
	}
	var out []LSStaticIPRow
	for _, r := range rows {
		if r.IsAttached && r.AttachedTo == instanceName {
			out = append(out, r)
		}
	}
	return out, nil
}

func lsEnsureStaticIP(ctx context.Context, cli *lightsail.Client, staticIPName string) error {
	_, err := cli.AllocateStaticIp(ctx, &lightsail.AllocateStaticIpInput{
		StaticIpName: aws.String(staticIPName),
	})
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "AlreadyExists") || strings.Contains(msg, "already exists") || strings.Contains(msg, "Name is already in use") {
			return nil
		}
		return err
	}
	return nil
}

func lsAttachStaticIP(ctx context.Context, cli *lightsail.Client, staticIPName, instanceName string) error {
	_, err := cli.AttachStaticIp(ctx, &lightsail.AttachStaticIpInput{
		StaticIpName: aws.String(staticIPName),
		InstanceName: aws.String(instanceName),
	})
	return err
}

func lsDetachStaticIP(ctx context.Context, cli *lightsail.Client, staticIPName string) error {
	_, err := cli.DetachStaticIp(ctx, &lightsail.DetachStaticIpInput{
		StaticIpName: aws.String(staticIPName),
	})
	return err
}

func lsReleaseStaticIP(ctx context.Context, cli *lightsail.Client, staticIPName string) error {
	_, err := cli.ReleaseStaticIp(ctx, &lightsail.ReleaseStaticIpInput{
		StaticIpName: aws.String(staticIPName),
	})
	return err
}

// lsCreate - 优化版：支持数字选择所有套餐和系统
func lsCreate(ctx context.Context, regions []string, creds aws.CredentialsProvider) {
	// 1. 选择区域
	region, err := pickFromList("\n选择 Lightsail Region：", regions, "us-east-1")
	if err != nil {
		fmt.Println("❌ 选择失败：", err)
		return
	}
	cfg, err := mkCfg(ctx, region, creds)
	if err != nil {
		fmt.Println("❌ 初始化失败：", err)
		return
	}
	cli := lightsail.NewFromConfig(cfg)

	// 2. 基础配置
	azDef := region + "a"
	az := input(fmt.Sprintf("可用区（如 %sa）[%s]: ", region, azDef), azDef)
	nameDef := "LS-" + region + "-1"
	name := input(fmt.Sprintf("实例名称 [%s]: ", nameDef), nameDef)

	openAll := yes(input("是否创建后全开端口（TCP/UDP 0-65535）？[y/N]: ", "n"))
	bindStatic := yes(input("是否创建后绑定静态IP（Static IPv4）？[y/N]: ", "n"))
	staticNameDef := "sip-" + name
	staticName := staticNameDef
	if bindStatic {
		staticName = input(fmt.Sprintf("静态IP名称 [%s]: ", staticNameDef), staticNameDef)
	}

	// 3. 选择套餐 (Bundle) - 显示全部
	fmt.Println("\n正在获取套餐列表...")
	bOut, err := cli.GetBundles(ctx, &lightsail.GetBundlesInput{})
	if err != nil {
		fmt.Println("❌ 获取套餐失败：", err)
		return
	}

	type bRow struct {
		ID    string
		Price float64
		Ram   float64
		Cpu   int32
	}
	var brs []bRow
	
	targetBundleDefault := "nano_3_0" 
	defaultBundleIdx := 1

	for _, b := range bOut.Bundles {
		// 过滤掉不可用 或 Windows
		if b.IsActive != nil && !*b.IsActive {
			continue
		}
		if b.SupportedPlatforms != nil && len(b.SupportedPlatforms) > 0 && b.SupportedPlatforms[0] == lst.InstancePlatformWindows {
			continue
		}

		price := 0.0
		if b.Price != nil {
			price = float64(*b.Price)
		}
		ram := 0.0
		if b.RamSizeInGb != nil {
			ram = float64(*b.RamSizeInGb)
		}

		brs = append(brs, bRow{
			ID:    aws.ToString(b.BundleId),
			Price: price,
			Ram:   ram,
			Cpu:   aws.ToInt32(b.CpuCount),
		})
	}
	// 按价格排序
	sort.Slice(brs, func(i, j int) bool { return brs[i].Price < brs[j].Price })

	// 查找默认值索引
	for i, b := range brs {
		if b.ID == targetBundleDefault {
			defaultBundleIdx = i + 1
			break
		}
	}

	fmt.Println("--- 所有可用 Linux 套餐 (按价格排序) ---")
	printTable("序号\tID\t价格\t内存\tCPU", func(w *tabwriter.Writer) {
		for i, b := range brs {
			marker := ""
			if i+1 == defaultBundleIdx {
				marker = " <-- 默认"
			}
			fmt.Fprintf(w, "[%d]\t%s\t$%.2f\t%.1f G\t%d vCPU%s\n", i+1, b.ID, b.Price, b.Ram, b.Cpu, marker)
		}
	})

	bInput := input(fmt.Sprintf("\n请输入套餐序号 (默认 %d 即 %s): ", defaultBundleIdx, brs[defaultBundleIdx-1].ID), "")
	finalBundleID := ""

	if bInput == "" {
		finalBundleID = brs[defaultBundleIdx-1].ID
	} else if idx, err := strconv.Atoi(bInput); err == nil {
		if idx >= 1 && idx <= len(brs) {
			finalBundleID = brs[idx-1].ID
		} else {
			fmt.Println("❌ 序号无效，使用默认值。")
			finalBundleID = brs[defaultBundleIdx-1].ID
		}
	} else {
		finalBundleID = strings.TrimSpace(bInput) // 允许直接粘贴 ID
	}
	fmt.Println("👉 已选套餐:", finalBundleID)

	// 4. 选择系统 (Blueprint) - 显示全部
	fmt.Println("\n正在获取系统镜像 (OS)...")
	pOut, err := cli.GetBlueprints(ctx, &lightsail.GetBlueprintsInput{})
	if err != nil {
		fmt.Println("❌ 获取镜像失败：", err)
		return
	}

	type osRow struct {
		ID      string
		Name    string
		Version string
	}
	var osList []osRow
	targetOSDefault := "debian_12" // 默认首选系统
	defaultOSIdx := 1

	for _, p := range pOut.Blueprints {
		// 修复常量：仅使用 LinuxUnix
		if p.Platform != lst.InstancePlatformLinuxUnix {
			continue
		}
		osList = append(osList, osRow{
			ID:      aws.ToString(p.BlueprintId),
			Name:    aws.ToString(p.Name),
			Version: aws.ToString(p.Version),
		})
	}
	
	// 按名称排序
	sort.Slice(osList, func(i, j int) bool { return osList[i].ID < osList[j].ID })

	// 查找默认值索引
	foundDefault := false
	for i, o := range osList {
		if o.ID == targetOSDefault {
			defaultOSIdx = i + 1
			foundDefault = true
			break
		}
	}
	// 如果没找到 debian_12，尝试找包含 debian 的
	if !foundDefault {
		for i, o := range osList {
			if strings.Contains(o.ID, "debian") {
				defaultOSIdx = i + 1
				break
			}
		}
	}

	fmt.Println("--- 所有可用 Linux 系统镜像 ---")
	printTable("序号\tID\t名称\t版本", func(w *tabwriter.Writer) {
		for i, o := range osList {
			marker := ""
			if i+1 == defaultOSIdx {
				marker = " <-- 默认"
			}
			fmt.Fprintf(w, "[%d]\t%s\t%s\t%s%s\n", i+1, o.ID, cut(o.Name, 25), cut(o.Version, 15), marker)
		}
	})

	osInput := input(fmt.Sprintf("\n请输入系统序号 (默认 %d 即 %s): ", defaultOSIdx, osList[defaultOSIdx-1].ID), "")
	finalBlueID := ""

	if osInput == "" {
		finalBlueID = osList[defaultOSIdx-1].ID
	} else if idx, err := strconv.Atoi(osInput); err == nil {
		if idx >= 1 && idx <= len(osList) {
			finalBlueID = osList[idx-1].ID
		} else {
			fmt.Println("❌ 序号无效，使用默认值。")
			finalBlueID = osList[defaultOSIdx-1].ID
		}
	} else {
		finalBlueID = strings.TrimSpace(osInput) // 允许直接粘贴 ID
	}
	fmt.Println("👉 已选系统:", finalBlueID)

	// 5. UserData
	rawUD, empty := collectUserData("\n可选：Lightsail 启动脚本 (UserData)")
	userData := ""
	if !empty {
		userData = rawUD
	}

	// 6. 执行创建
	fmt.Println("\n🚀 正在提交创建请求...")
	in := &lightsail.CreateInstancesInput{
		AvailabilityZone: aws.String(az),
		BlueprintId:      aws.String(finalBlueID),
		BundleId:         aws.String(finalBundleID),
		InstanceNames:    []string{name},
	}
	if userData != "" {
		in.UserData = aws.String(userData)
	}

	_, err = cli.CreateInstances(ctx, in)
	if err != nil {
		fmt.Println("❌ 创建失败:", err)
		if strings.Contains(err.Error(), "NotFoundException") {
			fmt.Println("💡 提示: 该套餐 ID 或系统 ID 可能在当前区域不存在，请尝试列表中的其他选项。")
		}
		return
	}
	fmt.Println("✅ 实例已创建:", name)

	// 7. 后续操作 (等待、开端口、绑IP)
	fmt.Println("⏳ 正在等待实例启动 (Running)...")
	if err := lsWaitRunning(ctx, cli, name, 10*time.Minute); err != nil {
		fmt.Println("⚠️ 等待超时:", err)
	}

	if openAll {
		fmt.Println("🔓 正在开放所有端口...")
		lsOpenAllPortsWithRetry(ctx, cli, name)
	}

	if bindStatic {
		fmt.Println("🌐 正在绑定静态 IP...")
		if err := lsEnsureStaticIP(ctx, cli, staticName); err == nil {
			lsAttachStaticIP(ctx, cli, staticName, name)
			fmt.Println("✅ 静态 IP 绑定完成:", staticName)
		} else {
			fmt.Println("❌ 静态 IP 操作失败:", err)
		}
	}
}

func lsControl(ctx context.Context, regions []string, creds aws.CredentialsProvider) {
RESELECT:
	rows, _ := lsListAll(ctx, regions, creds)
	if len(rows) == 0 {
		fmt.Println("❌ 未找到任何 Lightsail 实例。")
		return
	}

	fmt.Println("\n序号  区域          区域中文            名称                    状态        公网IP             可用区")
	for _, r := range rows {
		fmt.Printf("%-4d %-12s %-16s %-22s %-10s %-16s %s\n",
			r.Idx, r.Region, regionCN(r.Region), r.Name, r.State, r.IP, r.AZ)
	}

	pick := mustInt(input("\n请输入实例序号 IDX (0 返回主菜单): ", "0"))
	if pick == 0 {
		return
	}
	if pick < 1 || pick > len(rows) {
		fmt.Println("❌ 序号无效")
		goto RESELECT
	}
	sel := rows[pick-1]

	cfg, err := mkCfg(ctx, sel.Region, creds)
	if err != nil {
		fmt.Println("❌ 初始化失败:", err)
		return
	}
	cli := lightsail.NewFromConfig(cfg)

	for {
		o, e := cli.GetInstance(ctx, &lightsail.GetInstanceInput{InstanceName: &sel.Name})
		if e == nil && o.Instance != nil {
			ip := ""
			if o.Instance.PublicIpAddress != nil && *o.Instance.PublicIpAddress != "None" {
				ip = *o.Instance.PublicIpAddress
			}
			state := ""
			if o.Instance.State != nil {
				state = aws.ToString(o.Instance.State.Name)
			}
			fmt.Printf("\n当前选择: %s (%s) 状态=%s IP=%s\n", sel.Name, sel.Region, state, ip)

			attached, _ := lsFindStaticIPsAttachedTo(ctx, sel.Region, sel.Name, creds)
			if len(attached) > 0 {
				fmt.Println("已绑定的静态 IP:")
				for _, a := range attached {
					fmt.Printf(" - %s  IP=%s\n", a.Name, a.IP)
				}
			}
		}

		fmt.Println("\n1) 启动 (Start)")
		fmt.Println("2) 停止 (Stop)")
		fmt.Println("3) 重启 (Reboot)")
		fmt.Println("4) 刷新状态")
		fmt.Println("5) 创建并绑定静态 IP")
		fmt.Println("6) 解绑静态 IP")
		fmt.Println("7) 删除静态 IP")
		fmt.Println("9) 重新选择实例")
		fmt.Println("0) 返回主菜单")
		act := input("请选择 [4]: ", "4")

		var opErr error
		switch act {
		case "1":
			fmt.Println("🚀 正在启动...")
			_, opErr = cli.StartInstance(ctx, &lightsail.StartInstanceInput{InstanceName: &sel.Name})
		case "2":
			fmt.Println("🛑 正在停止...")
			_, opErr = cli.StopInstance(ctx, &lightsail.StopInstanceInput{InstanceName: &sel.Name})
		case "3":
			fmt.Println("🔁 正在重启...")
			_, opErr = cli.RebootInstance(ctx, &lightsail.RebootInstanceInput{InstanceName: &sel.Name})
		case "4":
			continue
		case "5": // Bind IP
			def := "sip-" + sel.Name
			sip := input(fmt.Sprintf("静态 IP 名称 [%s]: ", def), def)
			if sip != "" {
				lsEnsureStaticIP(ctx, cli, sip)
				opErr = lsAttachStaticIP(ctx, cli, sip, sel.Name)
				if opErr == nil {
					fmt.Println("✅ 已绑定:", sip)
				}
			}
		case "6": // Detach IP
			attached, _ := lsFindStaticIPsAttachedTo(ctx, sel.Region, sel.Name, creds)
			if len(attached) > 0 {
				fmt.Printf("正在解绑 %s...\n", attached[0].Name)
				opErr = lsDetachStaticIP(ctx, cli, attached[0].Name)
			} else {
				fmt.Println("当前无静态 IP。")
			}
		case "7": // Delete IP
			all, _ := lsListStaticIPsInRegion(ctx, sel.Region, creds)
			if len(all) == 0 {
				fmt.Println("该区域无静态 IP。")
				continue
			}
			for _, r := range all {
				att := r.AttachedTo
				if att == "" {
					att = "-"
				}
				fmt.Printf(" - %s (IP: %s) -> %s\n", r.Name, r.IP, att)
			}
			p := mustInt(input("输入要删除(释放)的静态IP编号 IDX（0 取消）: ", "0"))
			if p == 0 {
				continue
			}
			if p < 1 || p > len(all) {
				fmt.Println("❌ 编号无效")
				continue
			}
			sip := all[p-1]

			fmt.Println("⚠️ 删除静态IP不可逆：释放后该IP不再属于你")
			if !yes(input("确认删除？[y/N]: ", "n")) {
				fmt.Println("已取消")
				continue
			}

			if sip.IsAttached {
				fmt.Printf("该静态IP当前绑定到：%s\n", sip.AttachedTo)
				if !yes(input("是否先解绑再释放？[y/N]: ", "n")) {
					fmt.Println("已取消")
					continue
				}
				fmt.Println("🔓 DetachStaticIp...")
				if err := lsDetachStaticIP(ctx, cli, sip.Name); err != nil {
					fmt.Println("❌ 解绑失败：", err)
					continue
				}
				time.Sleep(2 * time.Second)
			}

			fmt.Println("🗑️ ReleaseStaticIp...")
			opErr = lsReleaseStaticIP(ctx, cli, sip.Name)
			if opErr == nil {
				fmt.Println("✅ 已释放静态IP：", sip.Name)
			}

		case "9":
			goto RESELECT
		case "0":
			return
		default:
			continue
		}

		if opErr != nil {
			fmt.Println("❌ 错误:", opErr)
		}
	}
}

// -------------------- EC2 逻辑 --------------------

// ec2ListAll - 并发扫描
func ec2ListAll(ctx context.Context, regions []string, creds aws.CredentialsProvider) ([]EC2InstanceRow, error) {
	var (
		mu   sync.Mutex
		rows = make([]EC2InstanceRow, 0, 16)
		wg   sync.WaitGroup
	)

	fmt.Printf("正在并发扫描 %d 个 EC2 区域...\n", len(regions))

	for _, rg := range regions {
		wg.Add(1)
		go func(region string) {
			defer wg.Done()

			cfg, err := mkCfg(ctx, region, creds)
			if err != nil {
				return
			}
			cli := ec2.NewFromConfig(cfg)

			out, err := cli.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
				Filters: []ec2t.Filter{
					{
						Name:   aws.String("instance-state-name"),
						Values: []string{"pending", "running", "stopping", "stopped", "shutting-down"},
					},
				},
			})
			if err != nil {
				return
			}

			var localRows []EC2InstanceRow
			for _, res := range out.Reservations {
				for _, ins := range res.Instances {
					name := ""
					for _, t := range ins.Tags {
						if t.Key != nil && *t.Key == "Name" && t.Value != nil {
							name = *t.Value
							break
						}
					}
					az := ""
					if ins.Placement.AvailabilityZone != nil {
						az = *ins.Placement.AvailabilityZone
					}
					pub := ""
					if ins.PublicIpAddress != nil {
						pub = *ins.PublicIpAddress
					}
					priv := ""
					if ins.PrivateIpAddress != nil {
						priv = *ins.PrivateIpAddress
					}
					state := string(ins.State.Name)
					typ := string(ins.InstanceType)

					localRows = append(localRows, EC2InstanceRow{
						Region: region,
						AZ:     az,
						ID:     aws.ToString(ins.InstanceId),
						State:  state,
						Name:   name,
						Type:   typ,
						PubIP:  pub,
						PrivIP: priv,
					})
				}
			}

			mu.Lock()
			rows = append(rows, localRows...)
			mu.Unlock()
		}(rg)
	}

	wg.Wait()

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Region == rows[j].Region {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].Region < rows[j].Region
	})

	for i := range rows {
		rows[i].Idx = i + 1
	}

	return rows, nil
}

func ec2Control(ctx context.Context, regions []string, creds aws.CredentialsProvider) {
RESELECT:
	rows, _ := ec2ListAll(ctx, regions, creds)
	if len(rows) == 0 {
		fmt.Println("❌ 未找到任何 EC2 实例。")
		return
	}

	fmt.Println("\n序号  区域          区域中文            AZ           实例ID                 状态        名称        类型       公网IP             内网IP")
	for _, r := range rows {
		fmt.Printf("%-4d %-12s %-16s %-12s %-20s %-9s %-10s %-9s %-16s %s\n",
			r.Idx, r.Region, regionCN(r.Region), r.AZ, r.ID, r.State, cut(r.Name, 10), r.Type, r.PubIP, r.PrivIP)
	}

	pick := mustInt(input("\n请输入实例序号 IDX (0 返回主菜单): ", "0"))
	if pick == 0 {
		return
	}
	if pick < 1 || pick > len(rows) {
		fmt.Println("❌ 序号无效")
		goto RESELECT
	}
	sel := rows[pick-1]

	cfg, err := mkCfg(ctx, sel.Region, creds)
	if err != nil {
		fmt.Println("❌ 初始化失败:", err)
		return
	}
	cli := ec2.NewFromConfig(cfg)

	for {
		o, e := cli.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{sel.ID}})
		stateNow := sel.State
		if e == nil && len(o.Reservations) > 0 {
			stateNow = string(o.Reservations[0].Instances[0].State.Name)
		}
		fmt.Printf("\n当前选择: %s (%s) 状态=%s\n", sel.ID, sel.Region, stateNow)

		fmt.Println("1) 启动 (Start)")
		fmt.Println("2) 停止 (Stop)")
		fmt.Println("3) 重启 (Reboot)")
		fmt.Println("4) 终止 (Terminate)")
		fmt.Println("5) 刷新状态")
		fmt.Println("9) 重新选择实例")
		fmt.Println("0) 返回主菜单")
		act := input("请选择 [5]: ", "5")

		var opErr error
		switch act {
		case "1":
			fmt.Println("🚀 正在启动...")
			_, opErr = cli.StartInstances(ctx, &ec2.StartInstancesInput{InstanceIds: []string{sel.ID}})
		case "2":
			fmt.Println("🛑 正在停止...")
			_, opErr = cli.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: []string{sel.ID}})
		case "3":
			fmt.Println("🔁 正在重启...")
			_, opErr = cli.RebootInstances(ctx, &ec2.RebootInstancesInput{InstanceIds: []string{sel.ID}})
		case "4":
			fmt.Println("⚠️ 警告：终止实例是不可逆的！")
			if yes(input("确认删除？[y/N]: ", "n")) {
				fmt.Println("🗑️ 正在终止...")
				_, opErr = cli.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{sel.ID}})
			} else {
				fmt.Println("已取消。")
			}
		case "5":
			continue
		case "9":
			goto RESELECT
		case "0":
			return
		default:
			continue
		}

		if opErr != nil {
			fmt.Println("❌ 错误:", opErr)
		}
	}
}

func ensureOpenAllSG(ctx context.Context, cli *ec2.Client, region string) (string, error) {
	vpcs, err := cli.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
		Filters: []ec2t.Filter{{Name: aws.String("isDefault"), Values: []string{"true"}}},
	})
	if err != nil {
		return "", err
	}
	if len(vpcs.Vpcs) == 0 || vpcs.Vpcs[0].VpcId == nil {
		return "", fmt.Errorf("未在 %s 找到默认 VPC", region)
	}
	vpcID := *vpcs.Vpcs[0].VpcId
	sgName := "open-all-ports"

	sgs, _ := cli.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: []ec2t.Filter{
			{Name: aws.String("group-name"), Values: []string{sgName}},
			{Name: aws.String("vpc-id"), Values: []string{vpcID}},
		},
	})
	if len(sgs.SecurityGroups) > 0 {
		sgID := *sgs.SecurityGroups[0].GroupId
		authorizeOpenAll(ctx, cli, sgID)
		return sgID, nil
	}

	created, err := cli.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(sgName),
		Description: aws.String("Open all TCP/UDP ports 0-65535"),
		VpcId:       aws.String(vpcID),
	})
	if err != nil {
		return "", err
	}
	sgID := *created.GroupId
	authorizeOpenAll(ctx, cli, sgID)
	return sgID, nil
}

func authorizeOpenAll(ctx context.Context, cli *ec2.Client, sgID string) error {
	_, err := cli.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []ec2t.IpPermission{
			{IpProtocol: aws.String("-1"), IpRanges: []ec2t.IpRange{{CidrIp: aws.String("0.0.0.0/0")}}},
		},
	})
	if err != nil && !strings.Contains(err.Error(), "InvalidPermission.Duplicate") {
		return err
	}
	return nil
}

func ec2Create(ctx context.Context, regions []string, creds aws.CredentialsProvider) {
	region, err := pickFromList("\n选择 EC2 Region：", regions, "us-east-1")
	if err != nil {
		fmt.Println("❌ 错误:", err)
		return
	}
	cfg, err := mkCfg(ctx, region, creds)
	if err != nil {
		fmt.Println("❌ 错误:", err)
		return
	}
	cli := ec2.NewFromConfig(cfg)

	ami := input("AMI ID (必须, 如 ami-xxxx): ", "")
	if ami == "" {
		fmt.Println("❌ 必须输入 AMI ID")
		return
	}
	itype := input("实例类型 [t3.micro]: ", "t3.micro")
	name := input("实例名称 (Name标签): ", "")
	openAll := yes(input("是否全开端口 (安全组)? [y/N]: ", "n"))

	rawUD, empty := collectUserData("\n可选：EC2 启动脚本 (UserData)")
	userDataB64 := ""
	if !empty {
		userDataB64 = base64.StdEncoding.EncodeToString([]byte(rawUD))
	}

	sgIds := []string{}
	if openAll {
		sgID, err := ensureOpenAllSG(ctx, cli, region)
		if err == nil {
			sgIds = append(sgIds, sgID)
			fmt.Println("✅ 使用安全组:", sgID)
		} else {
			fmt.Println("❌ 安全组错误:", err)
			return
		}
	}

	fmt.Println("\n🚀 正在启动实例...")
	runIn := &ec2.RunInstancesInput{
		ImageId:      aws.String(ami),
		InstanceType: ec2t.InstanceType(itype),
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
	}
	if len(sgIds) > 0 {
		runIn.SecurityGroupIds = sgIds
	}
	if userDataB64 != "" {
		runIn.UserData = aws.String(userDataB64)
	}

	out, err := cli.RunInstances(ctx, runIn)
	if err != nil {
		fmt.Println("❌ 启动失败:", err)
		return
	}
	id := *out.Instances[0].InstanceId
	fmt.Println("✅ 启动成功:", id)

	if name != "" {
		cli.CreateTags(ctx, &ec2.CreateTagsInput{
			Resources: []string{id},
			Tags:      []ec2t.Tag{{Key: aws.String("Name"), Value: aws.String(name)}},
		})
	}
}

// -------------------- Main --------------------

func main() {
	ctx := context.Background()

	fmt.Println("=== AWS 管理工具 (Go SDK) ===")
	fmt.Println("功能：EC2 / Lightsail 创建与管理\n")

	ak := input("AWS Access Key ID: ", "")
	sk := inputSecret("AWS Secret Access Key: ")
	if ak == "" || sk == "" {
		fmt.Println("❌ 必须输入 AK/SK")
		return
	}

	creds := credentials.NewStaticCredentialsProvider(ak, sk, "")

	fmt.Printf("\n🔍 正在验证凭证 (bootstrap=%s)...\n", bootstrapRegion)
	if err := stsCheck(ctx, bootstrapRegion, creds); err != nil {
		fmt.Println("❌ 凭证无效:", err)
		return
	}
	fmt.Println("✅ 验证成功。")

	fmt.Println("\n🌍 获取区域列表...")
	ec2Regions, _ := getEC2Regions(ctx, creds)
	lsRegions, _ := getLightsailRegions(ctx, creds)
	fmt.Printf("✅ EC2 区域: %d 个\n", len(ec2Regions))
	fmt.Printf("✅ Lightsail 区域: %d 个\n", len(lsRegions))

	for {
		fmt.Println("\n================ 主菜单 ================")
		fmt.Println("1) EC2：创建实例")
		fmt.Println("2) EC2：控制实例（全球并发扫描）")
		fmt.Println("3) Lightsail：创建实例")
		fmt.Println("4) Lightsail：控制实例（全球并发扫描）")
		fmt.Println("0) 退出")
		act := input("请选择 [0]: ", "0")

		switch act {
		case "1":
			ec2Create(ctx, ec2Regions, creds)
		case "2":
			ec2Control(ctx, ec2Regions, creds)
		case "3":
			lsCreate(ctx, lsRegions, creds)
		case "4":
			lsControl(ctx, lsRegions, creds)
		case "0":
			return
		default:
			fmt.Println("无效选项")
		}
	}
}
