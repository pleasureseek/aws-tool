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
	"github.com/aws/aws-sdk-go-v2/service/servicequotas"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

/*
功能：
- 运行 exe 后输入 AK/SK
- 主菜单：
  1) EC2：建实例 (自动配置 IPv6 网络/路由/安全组)
  2) EC2：控制实例
  3) Lightsail：建光帆
  4) Lightsail：控制光帆
  5) 查询配额 (隐藏区域显示)
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

// -------------------- 配额查询 --------------------

func checkQuotas(ctx context.Context, creds aws.CredentialsProvider) {
	// 依然使用 us-east-1 进行查询，但不显示出来
	region := "us-east-1"

	cfg, err := mkCfg(ctx, region, creds)
	if err != nil {
		fmt.Println("❌ 初始化失败：", err)
		return
	}

	// 修改点：只显示通用提示，不显示具体区域名
	fmt.Println("\n正在查询.....")

	sqCli := servicequotas.NewFromConfig(cfg)
	vcpuQuotaCode := "L-1216C47A" // 标准按需实例 vCPU 配额代码
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
		if val <= 0 {
			fmt.Println("   (提示: 配额为 0 通常意味着 EC2 未激活或被风控)")
		} else if val <= 32 {
			fmt.Println("   (提示: 新号通常限制在 32 vCPU)")
		}
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
				state := ""
				if ins.State != nil { state = aws.ToString(ins.State.Name) }
				az := ""
				if ins.Location != nil { az = aws.ToString(ins.Location.AvailabilityZone) }
				localRows = append(localRows, LSInstanceRow{
					Region: region, Name: aws.ToString(ins.Name), State: state, IP: ip, AZ: az,
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
	printTable("序号\t区域\t名称\t状态\tIP", func(w *tabwriter.Writer) {
		for _, r := range rows { fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", r.Idx, r.Region, r.Name, r.State, r.IP) }
	})
	
	// 简单删除逻辑演示
	if yes(input("\n是否需要删除某个实例? [y/N]: ", "n")) {
		idStr := input("请输入序号: ", "")
		idx := mustInt(idStr)
		if idx > 0 && idx <= len(rows) {
			sel := rows[idx-1]
			cfg, _ := mkCfg(ctx, sel.Region, creds)
			cli := lightsail.NewFromConfig(cfg)
			if yes(input(fmt.Sprintf("确认删除 %s (%s) 吗? [y/N]: ", sel.Name, sel.IP), "n")) {
				cli.DeleteInstance(ctx, &lightsail.DeleteInstanceInput{InstanceName: &sel.Name})
				fmt.Println("✅ 删除指令已发送")
			}
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
					
					local = append(local, EC2InstanceRow{
						Region: region, ID: *ins.InstanceId, State: string(ins.State.Name),
						Name: name, Type: string(ins.InstanceType), PubIP: pub,
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
	
	printTable("序号\t区域\tID\t名称\t状态\tIP", func(w *tabwriter.Writer) {
		for _, r := range rows { fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", r.Idx, r.Region, r.ID, r.Name, r.State, r.PubIP) }
	})

	idx := mustInt(input("\n输入序号操作 (0 返回): ", "0"))
	if idx <= 0 || idx > len(rows) { return }
	sel := rows[idx-1]
	
	cfg, _ := mkCfg(ctx, sel.Region, creds)
	cli := ec2.NewFromConfig(cfg)
	
	fmt.Printf("操作: %s\n1) 启动 2) 停止 3) 重启 4) 终止\n", sel.ID)
	switch input("选择: ", "0") {
	case "1": cli.StartInstances(ctx, &ec2.StartInstancesInput{InstanceIds: []string{sel.ID}}); fmt.Println("✅ 启动中")
	case "2": cli.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: []string{sel.ID}}); fmt.Println("✅ 停止中")
	case "3": cli.RebootInstances(ctx, &ec2.RebootInstancesInput{InstanceIds: []string{sel.ID}}); fmt.Println("✅ 重启中")
	case "4":
		if yes(input("⚠️ 确认终止实例 (删除)? [y/N]: ", "n")) {
			cli.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{sel.ID}})
			fmt.Println("🗑️ 正在终止...")
		}
	}
}

// 自动配置 IPv6 网络 (VPC -> Subnet -> Route)
func autoSetupIPv6(ctx context.Context, cli *ec2.Client, region, vpcID string) (string, error) {
	fmt.Println("🔍 正在检查/配置 IPv6 网络环境...")

	// 1. 检查 VPC 是否有 IPv6
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
		fmt.Println("   -> VPC 无 IPv6，正在申请亚马逊提供的 IPv6 CIDR...")
		_, err := cli.AssociateVpcCidrBlock(ctx, &ec2.AssociateVpcCidrBlockInput{
			VpcId: aws.String(vpcID), AmazonProvidedIpv6CidrBlock: aws.Bool(true),
		})
		if err != nil { return "", fmt.Errorf("申请 VPC IPv6 失败: %v", err) }
		
		// 等待分配
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

	// 2. 检查子网 (取默认子网列表)
	subOut, err := cli.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		Filters: []ec2t.Filter{{Name: aws.String("vpc-id"), Values: []string{vpcID}}},
	})
	if err != nil || len(subOut.Subnets) == 0 { return "", fmt.Errorf("找不到子网") }
	
	// 选第一个可用子网
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
		// 计算子网 CIDR (直接在 VPC /56 后面补 00 凑成 /64)
		// 简单字符串处理: 替换 /56 为 /64
		newSubnetCidr := strings.Replace(vpcCidrBlock, "/56", "/64", 1) 
		
		fmt.Printf("   -> 子网无 IPv6，正在分配 CIDR (%s)...\n", newSubnetCidr)
		_, err := cli.AssociateSubnetCidrBlock(ctx, &ec2.AssociateSubnetCidrBlockInput{
			SubnetId: aws.String(subnetID), Ipv6CidrBlock: aws.String(newSubnetCidr),
		})
		if err != nil {
			return "", fmt.Errorf("分配子网 IPv6 失败 (可能需手动配置): %v", err)
		}
		
		// 开启自动分配
		cli.ModifySubnetAttribute(ctx, &ec2.ModifySubnetAttributeInput{
			SubnetId: aws.String(subnetID), AssignIpv6AddressOnCreation: &ec2t.AttributeBooleanValue{Value: aws.Bool(true)},
		})
	}

	// 3. 检查路由表 (::/0 -> IGW)
	rtOut, err := cli.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{
		Filters: []ec2t.Filter{{Name: aws.String("vpc-id"), Values: []string{vpcID}}},
	})
	if err == nil && len(rtOut.RouteTables) > 0 {
		rt := rtOut.RouteTables[0]
		hasRoute := false
		var igwID string
		
		// 找 IGW
		igwOut, _ := cli.DescribeInternetGateways(ctx, &ec2.DescribeInternetGatewaysInput{
			Filters: []ec2t.Filter{{Name: aws.String("attachment.vpc-id"), Values: []string{vpcID}}},
		})
		if len(igwOut.InternetGateways) > 0 { igwID = *igwOut.InternetGateways[0].InternetGatewayId }

		for _, r := range rt.Routes {
			if aws.ToString(r.DestinationIpv6CidrBlock) == "::/0" {
				hasRoute = true
				break
			}
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
	// 获取默认 VPC
	vpcs, err := cli.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{Filters: []ec2t.Filter{{Name: aws.String("isDefault"), Values: []string{"true"}}}})
	if err != nil || len(vpcs.Vpcs) == 0 { return "", "", fmt.Errorf("默认 VPC 未找到") }
	vpcID := *vpcs.Vpcs[0].VpcId

	sgName := "open-all-ports"
	sgs, _ := cli.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: []ec2t.Filter{{Name: aws.String("group-name"), Values: []string{sgName}}, {Name: aws.String("vpc-id"), Values: []string{vpcID}}},
	})
	if len(sgs.SecurityGroups) > 0 { return *sgs.SecurityGroups[0].GroupId, vpcID, nil }

	// 创建新安全组
	res, err := cli.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{GroupName: aws.String(sgName), Description: aws.String("Auto generated"), VpcId: aws.String(vpcID)})
	if err != nil { return "", vpcID, err }
	
	// 放行 TCP/UDP 所有端口
	cli.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: res.GroupId,
		IpPermissions: []ec2t.IpPermission{
			{IpProtocol: aws.String("-1"), IpRanges: []ec2t.IpRange{{CidrIp: aws.String("0.0.0.0/0")}}}, // IPv4 All
			{IpProtocol: aws.String("-1"), Ipv6Ranges: []ec2t.Ipv6Range{{CidrIpv6: aws.String("::/0")}}}, // IPv6 All
		},
	})
	return *res.GroupId, vpcID, nil
}

func ec2Create(ctx context.Context, regions []string, creds aws.CredentialsProvider) {
	// 1. 基础信息
	region, err := pickFromList("\n选择 EC2 Region：", regions, "us-east-1")
	if err != nil { return }
	cfg, _ := mkCfg(ctx, region, creds)
	cli := ec2.NewFromConfig(cfg)

	ami := input("AMI ID (必须, 如 ami-xxxx): ", "")
	if ami == "" { fmt.Println("❌ AMI 不能为空"); return }
	
	itype := input("实例类型 [t3.micro]: ", "t3.micro")
	
	// 2. 新增：启动数量
	countStr := input("启动数量 [1]: ", "1")
	count := int32(mustInt(countStr))
	if count < 1 { count = 1 }

	// 3. 新增：磁盘大小
	var volSize int32
	diskStr := input("磁盘大小(GB) [默认]: ", "")
	if diskStr != "" {
		volSize = int32(mustInt(diskStr))
	}

	// 4. 新增：IPv6 开关
	enableIPv6 := yes(input("自动分配 IPv6 (自动修复 VPC)? [y/N]: ", "n"))

	// 5. 密码设置
	rootPwd := input("设置 SSH root 密码 (留空跳过): ", "")
	openAll := yes(input("全开端口 (安全组)? [y/N]: ", "n"))

	// 6. UserData 构造
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
	} else if !empty {
		userData = rawUD
	}
	
	// 准备安全组和 VPC ID
	var sgID, vpcID string
	if openAll || enableIPv6 {
		// 即使不全开端口，为了配 IPv6 也需要获取 vpcID
		s, v, err := ensureOpenAllSG(ctx, cli, region)
		if err != nil { fmt.Println("❌ 网络环境获取失败:", err); return }
		sgID = s
		vpcID = v
		if openAll { fmt.Println("✅ 使用安全组:", sgID) }
	}

	// 自动配置 IPv6
	var targetSubnetID string
	if enableIPv6 {
		sID, err := autoSetupIPv6(ctx, cli, region, vpcID)
		if err != nil {
			fmt.Println("⚠️ IPv6 配置失败 (将仅使用 IPv4):", err)
			enableIPv6 = false
		} else {
			targetSubnetID = sID
			fmt.Println("✅ IPv6 环境就绪，子网:", targetSubnetID)
		}
	}

	// 7. 构建请求
	runIn := &ec2.RunInstancesInput{
		ImageId: aws.String(ami), InstanceType: ec2t.InstanceType(itype),
		MinCount: aws.Int32(count), MaxCount: aws.Int32(count),
	}
	if userData != "" {
		runIn.UserData = aws.String(base64.StdEncoding.EncodeToString([]byte(userData)))
	}

	// 配置网络接口 (处理 IPv6 和 Subnet)
	if enableIPv6 || sgID != "" {
		netIf := ec2t.InstanceNetworkInterfaceSpecification{
			DeviceIndex: aws.Int32(0),
			AssociatePublicIpAddress: aws.Bool(true), // IPv4
		}
		if sgID != "" { netIf.Groups = []string{sgID} }
		if enableIPv6 {
			netIf.Ipv6AddressCount = aws.Int32(1)
			netIf.SubnetId = aws.String(targetSubnetID)
		}
		runIn.NetworkInterfaces = []ec2t.InstanceNetworkInterfaceSpecification{netIf}
	}

	// 处理磁盘大小
	if volSize > 0 {
		fmt.Println("🔍 查询 AMI 根设备名称...")
		imgOut, err := cli.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []string{ami}})
		if err == nil && len(imgOut.Images) > 0 {
			rootName := *imgOut.Images[0].RootDeviceName
			runIn.BlockDeviceMappings = []ec2t.BlockDeviceMapping{
				{
					DeviceName: aws.String(rootName),
					Ebs: &ec2t.EbsBlockDevice{
						VolumeSize: aws.Int32(volSize),
						VolumeType: ec2t.VolumeTypeGp3, 
					},
				},
			}
			fmt.Printf("✅ 磁盘将设为: %s %dGB\n", rootName, volSize)
		} else {
			fmt.Println("⚠️ 无法获取 AMI 信息，跳过磁盘调整")
		}
	}

	fmt.Printf("\n🚀 正在启动 %d 台实例...\n", count)
	out, err := cli.RunInstances(ctx, runIn)
	if err != nil {
		fmt.Println("❌ 启动失败:", err)
		return
	}
	
	for _, ins := range out.Instances {
		fmt.Println("✅ 成功:", *ins.InstanceId)
	}
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

	fmt.Println("🌍 获取区域...")
	ec2Regions, _ := getEC2Regions(ctx, creds)
	lsRegions, _ := getLightsailRegions(ctx, creds)

	for {
		fmt.Println("\n====== 主菜单 ======")
		fmt.Println("1) EC2：创建 (支持批量/磁盘/IPv6)")
		fmt.Println("2) EC2：管理 (全球扫描)")
		fmt.Println("3) Lightsail：创建")
		fmt.Println("4) Lightsail：管理")
		fmt.Println("5) 查询配额 (默认查 us-east-1)")
		fmt.Println("0) 退出")
		
		switch input("选择: ", "0") {
		case "1": ec2Create(ctx, ec2Regions, creds)
		case "2": ec2Control(ctx, ec2Regions, creds)
		case "3": lsCreate(ctx, lsRegions, creds)
		case "4": lsControl(ctx, lsRegions, creds)
		case "5": checkQuotas(ctx, creds)
		case "0": return
		}
	}
}
