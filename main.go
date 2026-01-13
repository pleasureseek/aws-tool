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
	"github.com/aws/aws-sdk-go-v2/service/account"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2t "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/lightsail"
	lst "github.com/aws/aws-sdk-go-v2/service/lightsail/types"
	"github.com/aws/aws-sdk-go-v2/service/servicequotas"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

/*
功能：
- 运行 exe 后输入 AK/SK
- 主菜单：
  1) EC2：建实例 (架构选择 + T2/T3/T4g 全系列 + 自动网络)
  2) EC2：控制实例 (终止时自动释放关联 EIP)
  3) Lightsail：建光帆
  4) Lightsail：控制光帆 (详情优化 + 管理功能)
  5) 查询配额
*/

const bootstrapRegion = "us-east-1"

// --- 数据结构 ---

type LSInstanceRow struct {
	Idx    int
	Region string
	Name   string
	State  string
	IP     string
	IPv6   string
	AZ     string
	Bundle string
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
	IPv6   string
}

type RegionInfo struct {
	Name   string
	Status string
}

type AMIOption struct {
	Name    string
	Owner   string
	Pattern string
}

type TypeOption struct {
	Type string
	Desc string
}

// -------------------- UI/辅助函数 --------------------

func regionCN(region string) string {
	m := map[string]string{
		"af-south-1":     "南非·开普敦",
		"ap-east-1":      "中国·香港",
		"ap-east-2":      "亚太·其他",
		"ap-northeast-1": "日本·东京",
		"ap-northeast-2": "韩国·首尔",
		"ap-northeast-3": "日本·大阪",
		"ap-south-1":     "印度·孟买",
		"ap-south-2":     "印度·海得拉巴",
		"ap-southeast-1": "新加坡",
		"ap-southeast-2": "澳大利亚·悉尼",
		"ap-southeast-3": "印度尼西亚·雅加达",
		"ap-southeast-4": "澳大利亚·墨尔本",
		"ap-southeast-5": "马来西亚·吉隆坡",
		"ap-southeast-6": "亚太·其他",
		"ap-southeast-7": "泰国·曼谷",
		"ca-central-1":   "加拿大·中部",
		"ca-west-1":      "加拿大·卡尔加里",
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
		"mx-central-1":   "墨西哥·中心",
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

func pickRegion(title string, items []RegionInfo, def string) (RegionInfo, error) {
	if len(items) == 0 {
		return RegionInfo{}, errors.New("列表为空")
	}
	fmt.Println(title)
	defIdx := 1
	for i := range items {
		if items[i].Name == def {
			defIdx = i + 1
			break
		}
	}
	for i, it := range items {
		statusMark := ""
		if it.Status == "not-opted-in" {
			statusMark = " [⚠️ 未启用]"
		} else if it.Status == "opted-in" {
			statusMark = " [已启用]"
		}
		fmt.Printf("  %2d) %-14s --- %s%s\n", i+1, it.Name, regionCN(it.Name), statusMark)
	}
	s := input(fmt.Sprintf("请输入编号 [%d]: ", defIdx), fmt.Sprintf("%d", defIdx))
	idx := mustInt(s)
	if idx < 1 || idx > len(items) {
		return RegionInfo{}, fmt.Errorf("编号无效")
	}
	return items[idx-1], nil
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

func printTable(header string, rowsFunc func(*tabwriter.Writer)) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, header)
	rowsFunc(w)
	w.Flush()
}

// -------------------- Regions 获取与启用 --------------------

func getEC2RegionsWithStatus(ctx context.Context, creds aws.CredentialsProvider) ([]RegionInfo, error) {
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
	var rs []RegionInfo
	for _, r := range out.Regions {
		if r.RegionName != nil && *r.RegionName != "" {
			rs = append(rs, RegionInfo{
				Name:   *r.RegionName,
				Status: aws.ToString(r.OptInStatus),
			})
		}
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].Name < rs[j].Name })
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

func ensureRegionOptIn(ctx context.Context, regionName, currentStatus string, creds aws.CredentialsProvider) error {
	if currentStatus == "opt-in-not-required" || currentStatus == "opted-in" {
		return nil
	}
	fmt.Printf("\n⚠️  检测到区域 %s 当前状态为 [%s] (未启用)\n", regionName, currentStatus)
	fmt.Println("注意：启用区域是 AWS 账户级别的操作，通常需要 5~20 分钟生效。")
	if !yes(input("是否立即发起启用请求并等待？[y/N]: ", "n")) {
		return fmt.Errorf("用户取消操作")
	}
	cfg, err := mkCfg(ctx, bootstrapRegion, creds)
	if err != nil {
		return err
	}
	acctCli := account.NewFromConfig(cfg)
	fmt.Printf("🚀 正在调用 EnableRegion (%s)...\n", regionName)
	_, err = acctCli.EnableRegion(ctx, &account.EnableRegionInput{
		RegionName: aws.String(regionName),
	})
	if err != nil {
		if !strings.Contains(err.Error(), "ResourceAlreadyExists") && !strings.Contains(err.Error(), "Region is enabled") {
			return fmt.Errorf("启用请求失败: %v", err)
		}
	}
	fmt.Println("⏳ 请求已发送，进入等待模式 (每 15 秒检查一次)...")
	fmt.Println("提示：您可以按 Ctrl+C 中止等待，稍后再试。")
	ec2Cli := ec2.NewFromConfig(cfg)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		<-ticker.C
		out, err := ec2Cli.DescribeRegions(ctx, &ec2.DescribeRegionsInput{
			RegionNames: []string{regionName},
			AllRegions:  aws.Bool(true),
		})
		if err != nil {
			fmt.Print("x")
			continue
		}
		if len(out.Regions) > 0 {
			status := aws.ToString(out.Regions[0].OptInStatus)
			fmt.Printf("[%s] ", status)
			if status == "opted-in" {
				fmt.Println("\n✅ 区域已成功启用！")
				return nil
			}
		}
	}
}

// -------------------- 配额查询 --------------------

func checkQuotas(ctx context.Context, creds aws.CredentialsProvider) {
	region := "us-east-1"
	cfg, err := mkCfg(ctx, region, creds)
	if err != nil {
		fmt.Println("❌ 初始化失败：", err)
		return
	}
	fmt.Println("\n正在查询.....")
	sqCli := servicequotas.NewFromConfig(cfg)
	vcpuQuotaCode := "L-1216C47A"
	vcpuServiceCode := "ec2"
	fmt.Print("🔍 查询 EC2 vCPU 配额... ")
	qOut, err := sqCli.GetServiceQuota(ctx, &servicequotas.GetServiceQuotaInput{
		ServiceCode: &vcpuServiceCode,
		QuotaCode:   &vcpuQuotaCode,
	})
	if err != nil {
		fmt.Printf("失败: %v\n", err)
	} else {
		val := 0.0
		if qOut.Quota != nil && qOut.Quota.Value != nil {
			val = *qOut.Quota.Value
		}
		fmt.Printf("✅ %.0f vCPU\n", val)
	}
	fmt.Print("🔍 查询 Lightsail 状态... ")
	lsCli := lightsail.NewFromConfig(cfg)
	_, lsErr := lsCli.GetInstances(ctx, &lightsail.GetInstancesInput{})
	if lsErr != nil {
		fmt.Printf("❌ 访问受限: %v\n", lsErr)
	} else {
		fmt.Println("✅ 服务正常")
	}
	input("\n按回车键返回主菜单...", "")
}

// -------------------- Lightsail --------------------

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
			if err != nil { return }
			cli := lightsail.NewFromConfig(cfg)
			out, err := cli.GetInstances(ctx, &lightsail.GetInstancesInput{})
			if err != nil || len(out.Instances) == 0 { return }
			var localRows []LSInstanceRow
			for _, ins := range out.Instances {
				ip := ""
				if ins.PublicIpAddress != nil { ip = *ins.PublicIpAddress }
				ipv6 := ""
				if len(ins.Ipv6Addresses) > 0 { ipv6 = ins.Ipv6Addresses[0] }
				state := ""
				if ins.State != nil { state = aws.ToString(ins.State.Name) }
				az := ""
				if ins.Location != nil { az = aws.ToString(ins.Location.AvailabilityZone) }
				bundle := ""
				if ins.BundleId != nil { bundle = *ins.BundleId }

				localRows = append(localRows, LSInstanceRow{
					Region: region, Name: aws.ToString(ins.Name), State: state, IP: ip, IPv6: ipv6, AZ: az, Bundle: bundle,
				})
			}
			mu.Lock()
			rows = append(rows, localRows...)
			mu.Unlock()
		}(rg)
	}
	wg.Wait()
	sort.Slice(rows, func(i, j int) bool { return rows[i].Region < rows[j].Region })
	for i := range rows { rows[i].Idx = i + 1 }
	return rows, nil
}

func lsCreate(ctx context.Context, regions []string, creds aws.CredentialsProvider) {
	region, err := pickFromList("\n选择 Lightsail Region：", regions, "us-east-1")
	if err != nil { return }
	cfg, _ := mkCfg(ctx, region, creds)
	cli := lightsail.NewFromConfig(cfg)
	az := input("可用区 (默认自动): ", region+"a")
	name := input("实例名称 [LS-1]: ", "LS-1")
	fmt.Println("\n正在获取套餐...")
	bOut, _ := cli.GetBundles(ctx, &lightsail.GetBundlesInput{})
	type bRow struct { ID string; Price float64; Ram float64; Cpu int32 }
	var brs []bRow
	defBundle := "nano_3_0"
	defIdx := 1
	for _, b := range bOut.Bundles {
		if b.IsActive != nil && !*b.IsActive { continue }
		if b.SupportedPlatforms != nil && len(b.SupportedPlatforms) > 0 && b.SupportedPlatforms[0] == lst.InstancePlatformWindows { continue }
		brs = append(brs, bRow{ID: *b.BundleId, Price: float64(*b.Price), Ram: float64(*b.RamSizeInGb), Cpu: *b.CpuCount})
	}
	sort.Slice(brs, func(i, j int) bool { return brs[i].Price < brs[j].Price })
	for i, b := range brs { if b.ID == defBundle { defIdx = i + 1; break } }
	fmt.Println("--- 套餐列表 ---")
	printTable("NO.\tID\tPrice\tRAM\tCPU", func(w *tabwriter.Writer) {
		for i, b := range brs {
			mk := ""; if i+1 == defIdx { mk = " <-- 默认" }
			fmt.Fprintf(w, "[%d]\t%s\t$%.2f\t%.1f G\t%d vCPU%s\n", i+1, b.ID, b.Price, b.Ram, b.Cpu, mk)
		}
	})
	bIn := input(fmt.Sprintf("输入套餐序号 (默认 %d): ", defIdx), "")
	finalBundle := brs[defIdx-1].ID
	if idx, err := strconv.Atoi(bIn); err == nil && idx > 0 && idx <= len(brs) { finalBundle = brs[idx-1].ID }
	fmt.Println("\n--- 系统列表 ---")
	pOut, _ := cli.GetBlueprints(ctx, &lightsail.GetBlueprintsInput{})
	var osList []string
	defOSIdx := 1
	for _, p := range pOut.Blueprints {
		if p.Platform == lst.InstancePlatformLinuxUnix {
			osList = append(osList, *p.BlueprintId)
		}
	}
	sort.Strings(osList)
	for i, os := range osList {
		mk := ""; if os == "debian_12" { mk = " <-- 默认"; defOSIdx = i+1 }
		fmt.Printf("[%d] %s%s\n", i+1, os, mk)
	}
	oIn := input(fmt.Sprintf("输入系统序号 (默认 %d): ", defOSIdx), "")
	finalOS := osList[defOSIdx-1]
	if idx, err := strconv.Atoi(oIn); err == nil && idx > 0 && idx <= len(osList) { finalOS = osList[idx-1] }
	ud, _ := collectUserData("\n可选：UserData 脚本")
	fmt.Println("🚀 创建中...")
	_, err = cli.CreateInstances(ctx, &lightsail.CreateInstancesInput{
		AvailabilityZone: aws.String(az), BlueprintId: aws.String(finalOS), BundleId: aws.String(finalBundle),
		InstanceNames: []string{name}, UserData: aws.String(ud),
	})
	if err != nil { fmt.Println("❌ 失败:", err); return }
	fmt.Println("✅ 已提交")
}

func lsControl(ctx context.Context, regions []string, creds aws.CredentialsProvider) {
	rows, _ := lsListAll(ctx, regions, creds)
	if len(rows) == 0 { fmt.Println("❌ 无实例"); return }
	
	// List View
	printTable("序号\t区域\t名称\t状态\t配置\tIPv4\tIPv6", func(w *tabwriter.Writer) {
		for _, r := range rows { fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Idx, r.Region, r.Name, r.State, cut(r.Bundle, 10), r.IP, r.IPv6) }
	})

	idx := mustInt(input("\n输入序号操作 (0 返回): ", "0"))
	if idx <= 0 || idx > len(rows) { return }
	sel := rows[idx-1]
	
	cfg, _ := mkCfg(ctx, sel.Region, creds)
	cli := lightsail.NewFromConfig(cfg)

	// Detail View (Fetch full details)
	fmt.Printf("\n🔍 正在获取 Lightsail 实例 %s 的详细指标...\n", sel.Name)
	insOut, err := cli.GetInstance(ctx, &lightsail.GetInstanceInput{InstanceName: &sel.Name})
	if err == nil && insOut.Instance != nil {
		ins := insOut.Instance
		var ports []string
		for _, p := range ins.Networking.Ports {
			if p.FromPort == 0 && (p.Protocol == "all" || p.Protocol == "-1") {
				ports = append(ports, "全部允许")
			} else {
				ports = append(ports, fmt.Sprintf("%d/%s", p.FromPort, p.Protocol))
			}
		}
		
		fmt.Println("================================================================")
		fmt.Printf(" 实例名称  : %s\n", *ins.Name)
		fmt.Printf(" 所在区域  : %s (%s)\n", sel.Region, *ins.Location.AvailabilityZone)
		fmt.Printf(" 套餐类型  : %s (%d vCPU, %.1f GB RAM)\n", *ins.BundleId, *ins.Hardware.CpuCount, *ins.Hardware.RamSizeInGb)
		if ins.Hardware.Disks != nil && len(ins.Hardware.Disks) > 0 {
			fmt.Printf(" 磁盘容量  : %d GB\n", *ins.Hardware.Disks[0].SizeInGb)
		}
		fmt.Printf(" 运行状态  : %s\n", *ins.State.Name)
		fmt.Printf(" 公网 IPv4 : %s\n", sel.IP)
		fmt.Printf(" 私网 IPv4 : %s\n", *ins.PrivateIpAddress)
		if sel.IPv6 != "" {
			fmt.Printf(" IPv6 地址 : %s\n", sel.IPv6)
		} else {
			fmt.Printf(" IPv6 地址 : (未开启)\n")
		}
		if ins.SshKeyName != nil {
			fmt.Printf(" SSH 密钥  : %s\n", *ins.SshKeyName)
		}
		fmt.Printf(" 开放端口  : %s\n", strings.Join(ports, ", "))
		fmt.Println("================================================================")
	}

	fmt.Printf("\n操作: %s\n1) 启动 2) 停止 3) 重启 4) 删除\n", sel.Name)
	switch input("选择: ", "0") {
	case "1": 
		_, err := cli.StartInstance(ctx, &lightsail.StartInstanceInput{InstanceName: &sel.Name})
		if err == nil { fmt.Println("✅ 启动中") } else { fmt.Println("❌ 失败:", err) }
	case "2": 
		_, err := cli.StopInstance(ctx, &lightsail.StopInstanceInput{InstanceName: &sel.Name})
		if err == nil { fmt.Println("✅ 停止中") } else { fmt.Println("❌ 失败:", err) }
	case "3": 
		_, err := cli.RebootInstance(ctx, &lightsail.RebootInstanceInput{InstanceName: &sel.Name})
		if err == nil { fmt.Println("✅ 重启中") } else { fmt.Println("❌ 失败:", err) }
	case "4":
		if yes(input("⚠️ 确认删除实例? [y/N]: ", "n")) {
			// 检查并询问是否释放静态 IP
			sipOut, err := cli.GetStaticIp(ctx, &lightsail.GetStaticIpInput{StaticIpName: aws.String("sip-" + sel.Name)})
			// 如果按照 sip-实例名 找不到，尝试遍历 region 所有静态IP
			if err != nil {
				allSip, _ := cli.GetStaticIps(ctx, &lightsail.GetStaticIpsInput{})
				for _, s := range allSip.StaticIps {
					if s.AttachedTo != nil && *s.AttachedTo == sel.Name {
						if yes(input(fmt.Sprintf("⚠️ 发现关联静态IP (%s)，是否释放? [y/N]: ", *s.Name), "n")) {
							cli.ReleaseStaticIp(ctx, &lightsail.ReleaseStaticIpInput{StaticIpName: s.Name})
							fmt.Println("🗑️ IP 已释放")
						}
						break
					}
				}
			} else if sipOut.StaticIp != nil {
				if yes(input(fmt.Sprintf("⚠️ 发现关联静态IP (%s)，是否释放? [y/N]: ", *sipOut.StaticIp.Name), "n")) {
					cli.ReleaseStaticIp(ctx, &lightsail.ReleaseStaticIpInput{StaticIpName: sipOut.StaticIp.Name})
					fmt.Println("🗑️ IP 已释放")
				}
			}

			_, err = cli.DeleteInstance(ctx, &lightsail.DeleteInstanceInput{InstanceName: &sel.Name})
			if err == nil { fmt.Println("🗑️ 实例删除中...") } else { fmt.Println("❌ 失败:", err) }
		}
	}
}

// -------------------- EC2 --------------------

func ec2ListAll(ctx context.Context, regions []string, creds aws.CredentialsProvider) ([]EC2InstanceRow, error) {
	var mu sync.Mutex
	var rows []EC2InstanceRow
	var wg sync.WaitGroup
	fmt.Printf("正在并发扫描 %d 个 EC2 区域...\n", len(regions))
	for _, rg := range regions {
		wg.Add(1)
		go func(region string) {
			defer wg.Done()
			cfg, err := mkCfg(ctx, region, creds)
			if err != nil { return }
			cli := ec2.NewFromConfig(cfg)
			out, err := cli.DescribeInstances(ctx, &ec2.DescribeInstancesInput{})
			if err != nil { return }
			var local []EC2InstanceRow
			for _, res := range out.Reservations {
				for _, ins := range res.Instances {
					if ins.State.Name == ec2t.InstanceStateNameTerminated { continue }
					name := ""
					for _, t := range ins.Tags { if *t.Key == "Name" { name = *t.Value } }
					pub := ""; if ins.PublicIpAddress != nil { pub = *ins.PublicIpAddress }
					priv := ""; if ins.PrivateIpAddress != nil { priv = *ins.PrivateIpAddress }
					
					ipv6 := ""
					if len(ins.NetworkInterfaces) > 0 && len(ins.NetworkInterfaces[0].Ipv6Addresses) > 0 {
						ipv6 = *ins.NetworkInterfaces[0].Ipv6Addresses[0].Ipv6Address
					}

					local = append(local, EC2InstanceRow{
						Region: region, ID: *ins.InstanceId, State: string(ins.State.Name),
						Name: name, Type: string(ins.InstanceType), PubIP: pub, PrivIP: priv, IPv6: ipv6,
					})
				}
			}
			mu.Lock()
			rows = append(rows, local...)
			mu.Unlock()
		}(rg)
	}
	wg.Wait()
	sort.Slice(rows, func(i, j int) bool { return rows[i].Region < rows[j].Region })
	for i := range rows { rows[i].Idx = i + 1 }
	return rows, nil
}

func ec2Control(ctx context.Context, regions []string, creds aws.CredentialsProvider) {
	rows, _ := ec2ListAll(ctx, regions, creds)
	if len(rows) == 0 { fmt.Println("❌ 无实例"); return }
	
	printTable("序号\t区域\tID\t名称\t状态\t配置\t公网IP\t内网IP\tIPv6", func(w *tabwriter.Writer) {
		for _, r := range rows {
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", 
				r.Idx, r.Region, r.ID, cut(r.Name, 10), r.State, r.Type, r.PubIP, r.PrivIP, r.IPv6) 
		}
	})

	idx := mustInt(input("\n输入序号操作 (0 返回): ", "0"))
	if idx <= 0 || idx > len(rows) { return }
	sel := rows[idx-1]
	
	cfg, _ := mkCfg(ctx, sel.Region, creds)
	cli := ec2.NewFromConfig(cfg)

	fmt.Printf("\n🔍 正在获取实例 %s 的详细指标 (磁盘/网络/密钥)...\n", sel.ID)
	desc, err := cli.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{sel.ID}})
	if err == nil && len(desc.Reservations) > 0 {
		ins := desc.Reservations[0].Instances[0]
		var diskInfo []string
		for _, bd := range ins.BlockDeviceMappings {
			if bd.Ebs != nil {
				volID := *bd.Ebs.VolumeId
				vOut, err := cli.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []string{volID}})
				if err == nil && len(vOut.Volumes) > 0 {
					diskInfo = append(diskInfo, fmt.Sprintf("%s [%d GB %s]", *bd.DeviceName, *vOut.Volumes[0].Size, vOut.Volumes[0].VolumeType))
				}
			}
		}
		fmt.Println("================================================================")
		fmt.Printf(" 实例 ID   : %s\n", *ins.InstanceId)
		fmt.Printf(" 所在区域  : %s (%s)\n", sel.Region, *ins.Placement.AvailabilityZone)
		fmt.Printf(" 实例类型  : %s\n", ins.InstanceType)
		fmt.Printf(" 运行状态  : %s\n", ins.State.Name)
		fmt.Printf(" 公网 IPv4 : %s\n", sel.PubIP)
		fmt.Printf(" 内网 IPv4 : %s\n", sel.PrivIP)
		if sel.IPv6 != "" {
			fmt.Printf(" IPv6 地址 : %s\n", sel.IPv6)
		} else {
			fmt.Printf(" IPv6 地址 : (未分配)\n")
		}
		fmt.Printf(" 启动时间  : %s\n", ins.LaunchTime.Format("2006-01-02 15:04:05"))
		if ins.KeyName != nil {
			fmt.Printf(" SSH 密钥  : %s\n", *ins.KeyName)
		}
		fmt.Printf(" 磁盘挂载  : %s\n", strings.Join(diskInfo, ", "))
		fmt.Println("================================================================")
	}
	
	fmt.Printf("\n操作: %s\n1) 启动 2) 停止 3) 重启 4) 终止\n", sel.ID)
	switch input("选择: ", "0") {
	case "1": cli.StartInstances(ctx, &ec2.StartInstancesInput{InstanceIds: []string{sel.ID}}); fmt.Println("✅ 启动中")
	case "2": cli.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: []string{sel.ID}}); fmt.Println("✅ 停止中")
	case "3": cli.RebootInstances(ctx, &ec2.RebootInstancesInput{InstanceIds: []string{sel.ID}}); fmt.Println("✅ 重启中")
	case "4":
		if yes(input("⚠️ 确认终止实例 (删除)? [y/N]: ", "n")) {
			// 新增：检查并释放 EIP
			fmt.Println("🔍 正在检查关联的弹性 IP (EIP)...")
			eipOut, err := cli.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{
				Filters: []ec2t.Filter{{Name: aws.String("instance-id"), Values: []string{sel.ID}}},
			})
			if err == nil && len(eipOut.Addresses) > 0 {
				fmt.Printf("⚠️ 发现 %d 个关联 EIP，正在释放以防止扣费...\n", len(eipOut.Addresses))
				for _, addr := range eipOut.Addresses {
					_, err := cli.ReleaseAddress(ctx, &ec2.ReleaseAddressInput{AllocationId: addr.AllocationId})
					if err == nil {
						fmt.Printf("   ✅ 已释放 IP: %s\n", *addr.PublicIp)
					} else {
						fmt.Printf("   ❌ 释放失败 IP: %s (%v)\n", *addr.PublicIp, err)
					}
				}
			} else {
				fmt.Println("   无关联 EIP。")
			}

			cli.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{sel.ID}})
			fmt.Println("🗑️ 正在终止...")
		}
	}
}

// 辅助：获取最新 AMI
func getLatestAMI(ctx context.Context, cli *ec2.Client, owner, namePattern string) string {
	out, err := cli.DescribeImages(ctx, &ec2.DescribeImagesInput{
		Owners: []string{owner},
		Filters: []ec2t.Filter{
			{Name: aws.String("name"), Values: []string{namePattern}},
			{Name: aws.String("architecture"), Values: []string{"x86_64"}},
			{Name: aws.String("virtualization-type"), Values: []string{"hvm"}},
		},
	})
	if err != nil || len(out.Images) == 0 { return "" }
	sort.Slice(out.Images, func(i, j int) bool { return *out.Images[i].CreationDate > *out.Images[j].CreationDate })
	return *out.Images[0].ImageId
}

// 适配不同架构的AMI获取逻辑
func getLatestAMIWithArch(ctx context.Context, cli *ec2.Client, owner, namePattern, arch string) string {
	out, err := cli.DescribeImages(ctx, &ec2.DescribeImagesInput{
		Owners: []string{owner},
		Filters: []ec2t.Filter{
			{Name: aws.String("name"), Values: []string{namePattern}},
			{Name: aws.String("architecture"), Values: []string{arch}},
			{Name: aws.String("virtualization-type"), Values: []string{"hvm"}},
		},
	})
	if err != nil || len(out.Images) == 0 { return "" }
	sort.Slice(out.Images, func(i, j int) bool { return *out.Images[i].CreationDate > *out.Images[j].CreationDate })
	return *out.Images[0].ImageId
}

func autoSetupIPv6(ctx context.Context, cli *ec2.Client, region, vpcID string) (string, error) {
	fmt.Println("🔍 正在检查/配置 IPv6 网络环境...")
	vpcOut, err := cli.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{VpcIds: []string{vpcID}})
	if err != nil { return "", err }
	hasVpcIPv6 := false
	var vpcCidrBlock string
	for _, assoc := range vpcOut.Vpcs[0].Ipv6CidrBlockAssociationSet {
		if assoc.Ipv6CidrBlockState.State == ec2t.VpcCidrBlockStateCodeAssociated {
			hasVpcIPv6 = true
			vpcCidrBlock = *assoc.Ipv6CidrBlock
			break
		}
	}
	if !hasVpcIPv6 {
		fmt.Println("   -> VPC 无 IPv6，正在申请亚马逊 IPv6 CIDR...")
		_, err := cli.AssociateVpcCidrBlock(ctx, &ec2.AssociateVpcCidrBlockInput{
			VpcId: aws.String(vpcID), AmazonProvidedIpv6CidrBlock: aws.Bool(true),
		})
		if err != nil { return "", fmt.Errorf("申请 VPC IPv6 失败: %v", err) }
		fmt.Print("   -> 等待分配...")
		for i := 0; i < 10; i++ {
			time.Sleep(3 * time.Second)
			v, _ := cli.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{VpcIds: []string{vpcID}})
			for _, a := range v.Vpcs[0].Ipv6CidrBlockAssociationSet {
				if a.Ipv6CidrBlockState.State == ec2t.VpcCidrBlockStateCodeAssociated {
					vpcCidrBlock = *a.Ipv6CidrBlock
					fmt.Println(" 成功:", vpcCidrBlock)
					goto VPC_READY
				}
			}
			fmt.Print(".")
		}
		return "", fmt.Errorf("等待 VPC IPv6 超时")
	}
VPC_READY:
	subOut, err := cli.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		Filters: []ec2t.Filter{{Name: aws.String("vpc-id"), Values: []string{vpcID}}},
	})
	if err != nil || len(subOut.Subnets) == 0 { return "", fmt.Errorf("找不到子网") }
	targetSubnet := subOut.Subnets[0]
	subnetID := *targetSubnet.SubnetId
	hasSubnetIPv6 := false
	for _, assoc := range targetSubnet.Ipv6CidrBlockAssociationSet {
		if assoc.Ipv6CidrBlockState.State == ec2t.SubnetCidrBlockStateCodeAssociated {
			hasSubnetIPv6 = true
			break
		}
	}
	if !hasSubnetIPv6 {
		newSubnetCidr := strings.Replace(vpcCidrBlock, "/56", "/64", 1) 
		fmt.Printf("   -> 子网无 IPv6，正在分配 CIDR (%s)...\n", newSubnetCidr)
		_, err := cli.AssociateSubnetCidrBlock(ctx, &ec2.AssociateSubnetCidrBlockInput{
			SubnetId: aws.String(subnetID), Ipv6CidrBlock: aws.String(newSubnetCidr),
		})
		if err != nil { return "", fmt.Errorf("分配子网 IPv6 失败: %v", err) }
		cli.ModifySubnetAttribute(ctx, &ec2.ModifySubnetAttributeInput{
			SubnetId: aws.String(subnetID), AssignIpv6AddressOnCreation: &ec2t.AttributeBooleanValue{Value: aws.Bool(true)},
		})
	}
	rtOut, err := cli.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{
		Filters: []ec2t.Filter{{Name: aws.String("vpc-id"), Values: []string{vpcID}}},
	})
	if err == nil && len(rtOut.RouteTables) > 0 {
		rt := rtOut.RouteTables[0]
		hasRoute := false
		var igwID string
		igwOut, _ := cli.DescribeInternetGateways(ctx, &ec2.DescribeInternetGatewaysInput{
			Filters: []ec2t.Filter{{Name: aws.String("attachment.vpc-id"), Values: []string{vpcID}}},
		})
		if len(igwOut.InternetGateways) > 0 { igwID = *igwOut.InternetGateways[0].InternetGatewayId }
		for _, r := range rt.Routes {
			if aws.ToString(r.DestinationIpv6CidrBlock) == "::/0" { hasRoute = true; break }
		}
		if !hasRoute && igwID != "" {
			fmt.Println("   -> 添加 IPv6 路由 (::/0 -> IGW)...")
			cli.CreateRoute(ctx, &ec2.CreateRouteInput{
				RouteTableId: rt.RouteTableId, DestinationIpv6CidrBlock: aws.String("::/0"), GatewayId: aws.String(igwID),
			})
		}
	}
	return subnetID, nil
}

func ensureOpenAllSG(ctx context.Context, cli *ec2.Client, region string) (string, string, error) {
	vpcs, err := cli.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{Filters: []ec2t.Filter{{Name: aws.String("isDefault"), Values: []string{"true"}}}})
	if err != nil || len(vpcs.Vpcs) == 0 { return "", "", fmt.Errorf("默认 VPC 未找到") }
	vpcID := *vpcs.Vpcs[0].VpcId
	sgName := "open-all-ports"
	sgs, _ := cli.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: []ec2t.Filter{{Name: aws.String("group-name"), Values: []string{sgName}}, {Name: aws.String("vpc-id"), Values: []string{vpcID}}},
	})
	if len(sgs.SecurityGroups) > 0 { return *sgs.SecurityGroups[0].GroupId, vpcID, nil }
	res, err := cli.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{GroupName: aws.String(sgName), Description: aws.String("Auto generated"), VpcId: aws.String(vpcID)})
	if err != nil { return "", vpcID, err }
	cli.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: res.GroupId,
		IpPermissions: []ec2t.IpPermission{
			{IpProtocol: aws.String("-1"), IpRanges: []ec2t.IpRange{{CidrIp: aws.String("0.0.0.0/0")}}}, 
			{IpProtocol: aws.String("-1"), Ipv6Ranges: []ec2t.Ipv6Range{{CidrIpv6: aws.String("::/0")}}},
		},
	})
	return *res.GroupId, vpcID, nil
}

func ec2Create(ctx context.Context, regions []RegionInfo, creds aws.CredentialsProvider) {
	// 1. 架构选择
	fmt.Println("\n请选择 CPU 架构:")
	fmt.Println("  1) x86_64 (Intel/AMD) [默认]")
	fmt.Println("  2) arm64 (Graviton)")
	archSel := input("请输入编号 [1]: ", "1")
	targetArch := "x86_64"
	if archSel == "2" { targetArch = "arm64" }

	regionInfo, err := pickRegion("\n选择 EC2 Region：", regions, "us-east-1")
	if err != nil { return }
	if err := ensureRegionOptIn(ctx, regionInfo.Name, regionInfo.Status, creds); err != nil {
		fmt.Println("❌ 区域不可用:", err); return
	}
	region := regionInfo.Name
	cfg, _ := mkCfg(ctx, region, creds)
	cli := ec2.NewFromConfig(cfg)

	// --- AMI 列表 (按架构自动匹配) ---
	amiList := []AMIOption{
		{"Debian 12", "136693071363", "debian-12-*"},
		{"Debian 11", "136693071363", "debian-11-*"},
		{"Ubuntu 24.04", "099720109477", "ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-*"},
		{"Ubuntu 22.04", "099720109477", "ubuntu/images/hvm-ssd/ubuntu-jammy-22.04-*"},
		{"Ubuntu 20.04", "099720109477", "ubuntu/images/hvm-ssd/ubuntu-focal-20.04-*"},
		{"Amazon Linux 2023", "137112412989", "al2023-ami-2023.*"},
		{"Amazon Linux 2", "137112412989", "amzn2-ami-hvm-*"},
		{"CentOS Stream 9", "125523088429", "CentOS-Stream-ec2-9-*"},
		{"Rocky Linux 9", "792107900819", "Rocky-9-EC2-Base-*"},
		{"AlmaLinux 9", "764336703387", "AlmaLinux OS 9*"},
		{"RHEL 9", "309956199498", "RHEL-9.*_HVM-*"},
		{"Fedora Cloud 41", "125523088429", "Fedora-Cloud-Base-41-*"},
		{"Arch Linux", "647457786197", "Arch-Linux-*-basic-*"},
		{"OpenSUSE Leap 15.5", "679593333241", "openSUSE-Leap-15-5-v*-hvm-ssd-*"},
		{"Kali Linux", "679593333241", "kali-last-snapshot-*"},
	}

	fmt.Printf("\n请选择操作系统 (%s):\n", targetArch)
	for i, a := range amiList {
		fmt.Printf("  %2d) %s\n", i+1, a.Name)
	}
	fmt.Println("  99) 自定义 AMI ID")

	var ami string
	sel := input("请输入编号 [1]: ", "1")
	
	if sel == "99" {
		ami = input("请输入 AMI ID: ", "")
	} else {
		idx := mustInt(sel)
		if idx > 0 && idx <= len(amiList) {
			target := amiList[idx-1]
			fmt.Printf("🔍 正在搜索 %s (%s) 的最新镜像...\n", target.Name, targetArch)
			ami = getLatestAMIWithArch(ctx, cli, target.Owner, target.Pattern, targetArch)
		} else {
			fmt.Println("❌ 编号无效")
			return
		}
	}

	if ami == "" { fmt.Println("❌ 未找到 AMI，请检查区域或架构兼容性"); return }
	fmt.Println("✅ 选中 AMI:", ami)

	// --- 实例类型列表 (分架构、按配置低到高排序) ---
	var typeList []TypeOption
	if targetArch == "x86_64" {
		typeList = []TypeOption{
			// T2 Family
			{"t2.nano", "1 vCPU, 0.5 GiB"},
			{"t2.micro", "1 vCPU, 1.0 GiB"},
			{"t2.small", "1 vCPU, 2.0 GiB"},
			{"t2.medium", "2 vCPU, 4.0 GiB"},
			{"t2.large", "2 vCPU, 8.0 GiB"},
			{"t2.xlarge", "4 vCPU, 16.0 GiB"},
			{"t2.2xlarge", "8 vCPU, 32.0 GiB"},
			// T3 Family
			{"t3.nano", "2 vCPU, 0.5 GiB"},
			{"t3.micro", "2 vCPU, 1.0 GiB"},
			{"t3.small", "2 vCPU, 2.0 GiB"},
			{"t3.medium", "2 vCPU, 4.0 GiB"},
			{"t3.large", "2 vCPU, 8.0 GiB"},
			{"t3.xlarge", "4 vCPU, 16.0 GiB"},
			{"t3.2xlarge", "8 vCPU, 32.0 GiB"},
			// C5/M5 (Optional high end)
			{"c5.large", "2 vCPU, 4.0 GiB"},
			{"m5.large", "2 vCPU, 8.0 GiB"},
		}
	} else {
		// ARM (T4g Family)
		typeList = []TypeOption{
			{"t4g.nano", "2 vCPU, 0.5 GiB"},
			{"t4g.micro", "2 vCPU, 1.0 GiB"},
			{"t4g.small", "2 vCPU, 2.0 GiB"},
			{"t4g.medium", "2 vCPU, 4.0 GiB"},
			{"t4g.large", "2 vCPU, 8.0 GiB"},
			{"t4g.xlarge", "4 vCPU, 16.0 GiB"},
			{"t4g.2xlarge", "8 vCPU, 32.0 GiB"},
			{"c6g.large", "2 vCPU, 4.0 GiB"},
			{"m6g.large", "2 vCPU, 8.0 GiB"},
		}
	}

	fmt.Printf("\n请选择实例类型 (%s):\n", targetArch)
	for i, t := range typeList {
		fmt.Printf("  %2d) %-12s - %s\n", i+1, t.Type, t.Desc)
	}
	fmt.Println("  99) 自定义类型 (如 c6i.metal)")

	var itype string
	tSel := input("请输入编号 [1]: ", "1") // 默认选1 (t2.nano or t4g.nano)
	if tSel == "99" {
		itype = input("请输入类型: ", "t3.micro")
	} else {
		idx := mustInt(tSel)
		if idx > 0 && idx <= len(typeList) {
			itype = typeList[idx-1].Type
		} else {
			itype = typeList[0].Type // 默认第一个
		}
	}
	fmt.Println("✅ 选中类型:", itype)

	count := int32(mustInt(input("启动数量 [1]: ", "1")))
	if count < 1 { count = 1 }
	var volSize int32
	if d := input("磁盘大小(GB) [默认]: ", ""); d != "" { volSize = int32(mustInt(d)) }
	enableIPv6 := yes(input("自动分配 IPv6? [y/N]: ", "n"))
	rootPwd := input("设置 SSH root 密码 (留空跳过): ", "")
	openAll := yes(input("全开端口 (安全组)? [y/N]: ", "n"))

	rawUD, empty := collectUserData("\n可选：EC2 启动脚本")
	userData := ""
	if rootPwd != "" {
		userData = fmt.Sprintf(`#!/bin/bash
echo "root:%s" | chpasswd
sed -i 's/^#PermitRootLogin.*/PermitRootLogin yes/' /etc/ssh/sshd_config
sed -i 's/^PermitRootLogin.*/PermitRootLogin yes/' /etc/ssh/sshd_config
sed -i 's/^#PasswordAuthentication.*/PasswordAuthentication yes/' /etc/ssh/sshd_config
sed -i 's/^PasswordAuthentication.*/PasswordAuthentication yes/' /etc/ssh/sshd_config
service sshd restart
service ssh restart
`, rootPwd)
		if !empty { userData += "\n" + rawUD }
	} else if !empty { userData = rawUD }
	
	var sgID, vpcID string
	if openAll || enableIPv6 {
		s, v, err := ensureOpenAllSG(ctx, cli, region)
		if err != nil { fmt.Println("❌ 网络错误:", err); return }
		sgID = s; vpcID = v
		if openAll { fmt.Println("✅ 安全组:", sgID) }
	}

	var targetSubnetID string
	if enableIPv6 {
		sID, err := autoSetupIPv6(ctx, cli, region, vpcID)
		if err != nil {
			fmt.Println("⚠️ IPv6 配置失败:", err)
			enableIPv6 = false
		} else {
			targetSubnetID = sID
			fmt.Println("✅ IPv6 环境就绪:", targetSubnetID)
		}
	}

	runIn := &ec2.RunInstancesInput{
		ImageId: aws.String(ami), InstanceType: ec2t.InstanceType(itype),
		MinCount: aws.Int32(count), MaxCount: aws.Int32(count),
	}
	if userData != "" { runIn.UserData = aws.String(base64.StdEncoding.EncodeToString([]byte(userData))) }

	if enableIPv6 || sgID != "" {
		netIf := ec2t.InstanceNetworkInterfaceSpecification{DeviceIndex: aws.Int32(0), AssociatePublicIpAddress: aws.Bool(true)}
		if sgID != "" { netIf.Groups = []string{sgID} }
		if enableIPv6 {
			netIf.Ipv6AddressCount = aws.Int32(1)
			netIf.SubnetId = aws.String(targetSubnetID)
		}
		runIn.NetworkInterfaces = []ec2t.InstanceNetworkInterfaceSpecification{netIf}
	}

	if volSize > 0 {
		imgOut, _ := cli.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []string{ami}})
		if len(imgOut.Images) > 0 {
			runIn.BlockDeviceMappings = []ec2t.BlockDeviceMapping{{
				DeviceName: imgOut.Images[0].RootDeviceName,
				Ebs: &ec2t.EbsBlockDevice{VolumeSize: aws.Int32(volSize), VolumeType: ec2t.VolumeTypeGp3},
			}}
			fmt.Printf("✅ 磁盘: %dGB\n", volSize)
		}
	}

	fmt.Printf("\n🚀 正在启动 %d 台...\n", count)
	out, err := cli.RunInstances(ctx, runIn)
	if err != nil { fmt.Println("❌ 失败:", err); return }
	for _, ins := range out.Instances { fmt.Println("✅ 成功:", *ins.InstanceId) }
}

// -------------------- Main --------------------

func main() {
	ctx := context.Background()
	fmt.Println("=== AWS 管理工具 (Win/Linux通用) ===")
	
	ak := input("AWS Access Key ID: ", "")
	sk := inputSecret("AWS Secret Access Key: ")
	if ak == "" || sk == "" { return }
	creds := credentials.NewStaticCredentialsProvider(ak, sk, "")

	fmt.Printf("\n🔍 验证凭证...\n")
	if err := stsCheck(ctx, bootstrapRegion, creds); err != nil {
		fmt.Println("❌ 失败:", err)
		return
	}
	fmt.Println("✅ 成功")

	fmt.Println("🌍 获取区域列表...")
	ec2Regions, _ := getEC2RegionsWithStatus(ctx, creds)
	lsRegions, _ := getLightsailRegions(ctx, creds)

	for {
		fmt.Println("\n====== 主菜单 ======")
		fmt.Println("1) EC2：创建 (自动AMI/IPv6/磁盘)")
		fmt.Println("2) EC2：管理 (全球扫描)")
		fmt.Println("3) Lightsail：创建")
		fmt.Println("4) Lightsail：管理")
		fmt.Println("5) 查询配额")
		fmt.Println("0) 退出")
		
		switch input("选择: ", "0") {
		case "1": ec2Create(ctx, ec2Regions, creds)
		case "2": 
			var plainRegions []string
			for _, r := range ec2Regions { plainRegions = append(plainRegions, r.Name) }
			ec2Control(ctx, plainRegions, creds)
		case "3": lsCreate(ctx, lsRegions, creds)
		case "4": lsControl(ctx, lsRegions, creds)
		case "5": checkQuotas(ctx, creds)
		case "0": return
		}
	}
}
