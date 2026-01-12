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
- 运行 exe 后输入 AK/SK（可选 SessionToken）
- 主菜单：
  1) EC2：建实例（可选全开端口 + 可选 user-data）
  2) EC2：控制实例（扫描所有 region）
  3) Lightsail：建光帆（可选全开端口 + 可选 user-data）
  4) Lightsail：控制光帆（扫描所有 region）
*/

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
	// 简化：Windows 控制台不一定能隐藏输入（需要真正隐藏我再给 x/term 版本）
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
	fmt.Println("（直接回车跳过；多行输入用 END 结束）")
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
		return "", errors.New("empty list")
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
		fmt.Printf("  %2d) %s\n", i+1, it)
	}
	s := input(fmt.Sprintf("请输入编号 [%d]: ", defIdx), fmt.Sprintf("%d", defIdx))
	idx := mustInt(s)
	if idx < 1 || idx > len(items) {
		return "", fmt.Errorf("编号无效")
	}
	return items[idx-1], nil
}

func getEC2Regions(ctx context.Context, bootstrap string, creds aws.CredentialsProvider) ([]string, error) {
	cfg, err := mkCfg(ctx, bootstrap, creds)
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

func getLightsailRegions(ctx context.Context, bootstrap string, creds aws.CredentialsProvider) ([]string, error) {
	cfg, err := mkCfg(ctx, bootstrap, creds)
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
		name := string(r.Name) // string alias
		if name != "" {
			rs = append(rs, name)
		}
	}
	sort.Strings(rs)
	return rs, nil
}

//
// -------------------- Lightsail --------------------
//

func lsListAll(ctx context.Context, regions []string, creds aws.CredentialsProvider) ([]LSInstanceRow, error) {
	rows := make([]LSInstanceRow, 0, 8)
	idx := 0
	for _, rg := range regions {
		cfg, err := mkCfg(ctx, rg, creds)
		if err != nil {
			continue
		}
		cli := lightsail.NewFromConfig(cfg)
		out, err := cli.GetInstances(ctx, &lightsail.GetInstancesInput{})
		if err != nil {
			continue
		}
		for _, ins := range out.Instances {
			idx++
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
			rows = append(rows, LSInstanceRow{
				Idx:    idx,
				Region: rg,
				Name:   aws.ToString(ins.Name),
				State:  state,
				IP:     ip,
				AZ:     az,
			})
		}
	}
	return rows, nil
}

func lsControl(ctx context.Context, regions []string, creds aws.CredentialsProvider) {
	rows, _ := lsListAll(ctx, regions, creds)
	if len(rows) == 0 {
		fmt.Println("❌ 没找到任何 Lightsail 实例（或权限不足：lightsail:GetInstances）")
		return
	}

	fmt.Println("\nIDX  REGION        NAME                    STATE      PUBLIC_IP         AZ")
	for _, r := range rows {
		fmt.Printf("%-4d %-12s %-22s %-10s %-16s %s\n",
			r.Idx, r.Region, r.Name, r.State, r.IP, r.AZ)
	}

	pick := mustInt(input("\n输入要操作的实例编号 IDX: ", ""))
	if pick < 1 || pick > len(rows) {
		fmt.Println("❌ 编号无效")
		return
	}
	sel := rows[pick-1]
	fmt.Printf("\n已选择：%s (%s) state=%s\n\n", sel.Name, sel.Region, sel.State)

	cfg, err := mkCfg(ctx, sel.Region, creds)
	if err != nil {
		fmt.Println("❌ 初始化失败：", err)
		return
	}
	cli := lightsail.NewFromConfig(cfg)

	fmt.Println("1) 启动 start")
	fmt.Println("2) 停止 stop")
	fmt.Println("3) 重启 reboot")
	fmt.Println("4) 刷新状态")
	fmt.Println("0) 返回")
	act := input("请选择 [0]: ", "0")

	switch act {
	case "1":
		fmt.Println("🚀 启动中...")
		_, err = cli.StartInstance(ctx, &lightsail.StartInstanceInput{InstanceName: &sel.Name})
	case "2":
		fmt.Println("🛑 停止中...")
		_, err = cli.StopInstance(ctx, &lightsail.StopInstanceInput{InstanceName: &sel.Name})
	case "3":
		fmt.Println("🔁 重启中...")
		_, err = cli.RebootInstance(ctx, &lightsail.RebootInstanceInput{InstanceName: &sel.Name})
	case "4":
		o, e := cli.GetInstance(ctx, &lightsail.GetInstanceInput{InstanceName: &sel.Name})
		if e != nil {
			err = e
		} else {
			ip := ""
			if o.Instance != nil && o.Instance.PublicIpAddress != nil && *o.Instance.PublicIpAddress != "None" {
				ip = *o.Instance.PublicIpAddress
			}
			state := ""
			if o.Instance != nil && o.Instance.State != nil {
				state = aws.ToString(o.Instance.State.Name)
			}
			fmt.Printf("Name=%s  State=%s  IP=%s  Region=%s\n", sel.Name, state, ip, sel.Region)
		}
	case "0":
		return
	default:
		fmt.Println("无效选项")
		return
	}

	if err != nil {
		fmt.Println("❌ 操作失败：", err)
		fmt.Println("提示：AccessDenied 说明缺 lightsail:Start/Stop/Reboot 权限")
	} else {
		fmt.Println("✅ 操作已提交")
	}
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
	return fmt.Errorf("等待 running 超时")
}

func lsOpenAllPortsWithRetry(ctx context.Context, cli *lightsail.Client, name string) error {
	// 常见错误：instance in transition（pending），所以重试
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
		// 等待后重试
		time.Sleep(6 * time.Second)
		if i == 20 {
			return err
		}
	}
	return fmt.Errorf("unknown")
}

func lsCreate(ctx context.Context, regions []string, creds aws.CredentialsProvider) {
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

	azDef := region + "a"
	az := input(fmt.Sprintf("可用区（如 %sa）[%s]: ", region, azDef), azDef)
	nameDef := "LS-" + region + "-1"
	name := input(fmt.Sprintf("实例名称 [%s]: ", nameDef), nameDef)

	// 端口是否全开（可选）
	openAll := yes(input("是否创建后全开端口（TCP/UDP 0-65535 对公网）？[y/N]: ", "n"))

	fmt.Println("\n获取 bundle（套餐）...")
	bOut, err := cli.GetBundles(ctx, &lightsail.GetBundlesInput{})
	if err != nil {
		fmt.Println("❌ GetBundles 失败：", err)
		return
	}

	type bRow struct {
		ID    string
		Price float64
		Ram   float64
		Cpu   int32
		Disk  int32
	}
	brs := make([]bRow, 0, len(bOut.Bundles))
	for _, b := range bOut.Bundles {
		price := 0.0
		if b.Price != nil {
			price = float64(*b.Price) // *float32
		}
		ram := 0.0
		if b.RamSizeInGb != nil {
			ram = float64(*b.RamSizeInGb) // *float32
		}
		brs = append(brs, bRow{
			ID:    aws.ToString(b.BundleId),
			Price: price,
			Ram:   ram,
			Cpu:   aws.ToInt32(b.CpuCount),
			Disk:  aws.ToInt32(b.DiskSizeInGb),
		})
	}
	sort.Slice(brs, func(i, j int) bool { return brs[i].Price < brs[j].Price })

	fmt.Println("（展示前 30 个，按价格从低到高）")
	for i := 0; i < len(brs) && i < 30; i++ {
		fmt.Printf("  %2d) %-20s $%-6.2f ram=%.1f cpu=%d disk=%d\n",
			i+1, brs[i].ID, brs[i].Price, brs[i].Ram, brs[i].Cpu, brs[i].Disk)
	}
	bundle := input("输入 bundleId（直接粘贴，例如 nano_3_0）: ", "")
	if bundle == "" {
		fmt.Println("❌ bundleId 不能为空")
		return
	}

	fmt.Println("\n获取 blueprint（系统镜像）...")
	pOut, err := cli.GetBlueprints(ctx, &lightsail.GetBlueprintsInput{})
	if err != nil {
		fmt.Println("❌ GetBlueprints 失败：", err)
		return
	}
	max := 40
	if len(pOut.Blueprints) < max {
		max = len(pOut.Blueprints)
	}
	fmt.Println("（展示前 40 个）")
	for i := 0; i < max; i++ {
		p := pOut.Blueprints[i]
		fmt.Printf("  %2d) %-28s  %-10s  %s %s\n",
			i+1,
			aws.ToString(p.BlueprintId),
			string(p.Platform), // enum -> string
			aws.ToString(p.Name),
			aws.ToString(p.Version),
		)
	}
	blue := input("输入 blueprintId（建议 Ubuntu/Debian）: ", "")
	if blue == "" {
		fmt.Println("❌ blueprintId 不能为空")
		return
	}

	rawUD, empty := collectUserData("\n可选：Lightsail UserData 初始脚本")
	userData := ""
	if !empty {
		userData = rawUD // Lightsail 明文即可
	}

	fmt.Println("\n🚀 创建 Lightsail 实例...")
	in := &lightsail.CreateInstancesInput{
		AvailabilityZone: aws.String(az),
		BlueprintId:      aws.String(blue),
		BundleId:         aws.String(bundle),
		InstanceNames:    []string{name},
	}
	if userData != "" {
		in.UserData = aws.String(userData)
	}
	_, err = cli.CreateInstances(ctx, in)
	if err != nil {
		fmt.Println("❌ CreateInstances 失败：", err)
		return
	}
	fmt.Println("✅ 已提交创建请求：", name)

	fmt.Println("⏳ 等待 running...")
	if err := lsWaitRunning(ctx, cli, name, 10*time.Minute); err != nil {
		fmt.Println("⚠️ 等待 running 超时：", err)
	}

	if openAll {
		fmt.Println("🔓 全开端口中（带重试）...")
		if err := lsOpenAllPortsWithRetry(ctx, cli, name); err != nil {
			fmt.Println("⚠️ 全开端口失败：", err)
			fmt.Println("（可能是权限不足 lightsail:PutInstancePublicPorts，或实例仍在过渡）")
		} else {
			fmt.Println("✅ 端口已全开")
		}
	} else {
		fmt.Println("（未选择全开端口）")
	}

	o, _ := cli.GetInstance(ctx, &lightsail.GetInstanceInput{InstanceName: &name})
	if o != nil && o.Instance != nil {
		ip := ""
		if o.Instance.PublicIpAddress != nil {
			ip = *o.Instance.PublicIpAddress
		}
		state := ""
		if o.Instance.State != nil {
			state = aws.ToString(o.Instance.State.Name)
		}
		fmt.Printf("📡 %s  state=%s  ip=%s  az=%s\n", name, state, ip, az)
	}
}

//
// -------------------- EC2 --------------------
//

func ec2ListAll(ctx context.Context, regions []string, creds aws.CredentialsProvider) ([]EC2InstanceRow, error) {
	rows := make([]EC2InstanceRow, 0, 16)
	idx := 0

	for _, rg := range regions {
		cfg, err := mkCfg(ctx, rg, creds)
		if err != nil {
			continue
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
			continue
		}

		for _, res := range out.Reservations {
			for _, ins := range res.Instances {
				idx++
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

				rows = append(rows, EC2InstanceRow{
					Idx:    idx,
					Region: rg,
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
	}

	return rows, nil
}

func ec2Control(ctx context.Context, regions []string, creds aws.CredentialsProvider) {
	rows, _ := ec2ListAll(ctx, regions, creds)
	if len(rows) == 0 {
		fmt.Println("❌ 没找到任何 EC2 实例（或权限不足：ec2:DescribeInstances）")
		return
	}

	fmt.Println("\nIDX  REGION        AZ            INSTANCE_ID           STATE     NAME        TYPE      PUBLIC_IP         PRIVATE_IP")
	for _, r := range rows {
		fmt.Printf("%-4d %-12s %-12s %-20s %-9s %-10s %-9s %-16s %s\n",
			r.Idx, r.Region, r.AZ, r.ID, r.State, cut(r.Name, 10), r.Type, r.PubIP, r.PrivIP)
	}

	pick := mustInt(input("\n输入要操作的实例编号 IDX: ", ""))
	if pick < 1 || pick > len(rows) {
		fmt.Println("❌ 编号无效")
		return
	}
	sel := rows[pick-1]

	cfg, err := mkCfg(ctx, sel.Region, creds)
	if err != nil {
		fmt.Println("❌ 初始化失败：", err)
		return
	}
	cli := ec2.NewFromConfig(cfg)

	fmt.Printf("\n已选择：%s (%s) state=%s\n\n", sel.ID, sel.Region, sel.State)
	fmt.Println("1) 启动 start")
	fmt.Println("2) 停止 stop")
	fmt.Println("3) 重启 reboot")
	fmt.Println("4) 终止 terminate（不可逆）")
	fmt.Println("5) 刷新状态")
	fmt.Println("0) 返回")
	act := input("请选择 [0]: ", "0")

	switch act {
	case "1":
		fmt.Println("🚀 启动中...")
		_, err = cli.StartInstances(ctx, &ec2.StartInstancesInput{InstanceIds: []string{sel.ID}})
	case "2":
		fmt.Println("🛑 停止中...")
		_, err = cli.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: []string{sel.ID}})
	case "3":
		fmt.Println("🔁 重启中...")
		_, err = cli.RebootInstances(ctx, &ec2.RebootInstancesInput{InstanceIds: []string{sel.ID}})
	case "4":
		fmt.Println("⚠️ 终止不可逆：running/stopped -> shutting-down -> terminated")
		confirm := input("确认请输入 DELETE: ", "")
		if confirm != "DELETE" {
			fmt.Println("已取消")
			return
		}
		fmt.Println("🗑️ 终止中...")
		_, err = cli.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{sel.ID}})
	case "5":
		o, e := cli.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{sel.ID}})
		if e != nil {
			err = e
		} else {
			state := ""
			pub := ""
			if len(o.Reservations) > 0 && len(o.Reservations[0].Instances) > 0 {
				ins := o.Reservations[0].Instances[0]
				state = string(ins.State.Name)
				if ins.PublicIpAddress != nil {
					pub = *ins.PublicIpAddress
				}
			}
			fmt.Printf("Instance=%s  state=%s  public_ip=%s  region=%s\n", sel.ID, state, pub, sel.Region)
		}
	case "0":
		return
	default:
		fmt.Println("无效选项")
		return
	}

	if err != nil {
		fmt.Println("❌ 操作失败：", err)
		fmt.Println("提示：AccessDenied 说明缺 ec2:Start/Stop/Reboot/Terminate 权限")
	} else {
		fmt.Println("✅ 操作已提交")
	}
}

func ensureOpenAllSG(ctx context.Context, cli *ec2.Client, region string) (string, error) {
	// 找 default VPC
	vpcs, err := cli.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
		Filters: []ec2t.Filter{{Name: aws.String("isDefault"), Values: []string{"true"}}},
	})
	if err != nil {
		return "", err
	}
	if len(vpcs.Vpcs) == 0 || vpcs.Vpcs[0].VpcId == nil {
		return "", fmt.Errorf("未找到 default VPC（region=%s）", region)
	}
	vpcID := *vpcs.Vpcs[0].VpcId

	sgName := "open-all-ports"

	// 已存在就复用
	sgs, _ := cli.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: []ec2t.Filter{
			{Name: aws.String("group-name"), Values: []string{sgName}},
			{Name: aws.String("vpc-id"), Values: []string{vpcID}},
		},
	})
	if len(sgs.SecurityGroups) > 0 && sgs.SecurityGroups[0].GroupId != nil {
		sgID := *sgs.SecurityGroups[0].GroupId
		_ = authorizeOpenAll(ctx, cli, sgID)
		return sgID, nil
	}

	created, err := cli.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(sgName),
		Description: aws.String("Open all TCP/UDP ports (0-65535) to 0.0.0.0/0"),
		VpcId:       aws.String(vpcID),
	})
	if err != nil {
		return "", err
	}
	if created.GroupId == nil {
		return "", fmt.Errorf("CreateSecurityGroup 未返回 GroupId")
	}
	sgID := *created.GroupId

	if err := authorizeOpenAll(ctx, cli, sgID); err != nil {
		return "", err
	}
	return sgID, nil
}

func authorizeOpenAll(ctx context.Context, cli *ec2.Client, sgID string) error {
	_, err := cli.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []ec2t.IpPermission{
			{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int32(0),
				ToPort:     aws.Int32(65535),
				IpRanges:   []ec2t.IpRange{{CidrIp: aws.String("0.0.0.0/0")}},
			},
			{
				IpProtocol: aws.String("udp"),
				FromPort:   aws.Int32(0),
				ToPort:     aws.Int32(65535),
				IpRanges:   []ec2t.IpRange{{CidrIp: aws.String("0.0.0.0/0")}},
			},
		},
	})
	if err != nil {
		if strings.Contains(err.Error(), "InvalidPermission.Duplicate") {
			return nil
		}
		return err
	}
	return nil
}

func ec2Create(ctx context.Context, regions []string, creds aws.CredentialsProvider) {
	region, err := pickFromList("\n选择 EC2 Region：", regions, "us-east-1")
	if err != nil {
		fmt.Println("❌ 选择失败：", err)
		return
	}
	cfg, err := mkCfg(ctx, region, creds)
	if err != nil {
		fmt.Println("❌ 初始化失败：", err)
		return
	}
	cli := ec2.NewFromConfig(cfg)

	ami := input("AMI ID（必须，例如 ami-xxxxxxxx）: ", "")
	if ami == "" {
		fmt.Println("❌ AMI 不能为空")
		return
	}
	itype := input("Instance Type [t3.micro]: ", "t3.micro")
	name := input("Name 标签（可空）: ", "")

	openAll := yes(input("是否创建/使用安全组并全开端口（TCP/UDP 0-65535 对公网）？[y/N]: ", "n"))

	rawUD, empty := collectUserData("\n可选：EC2 UserData 启动脚本（注意：EC2 会自动 Base64）")
	userDataB64 := ""
	if !empty {
		userDataB64 = base64.StdEncoding.EncodeToString([]byte(rawUD))
	}

	sgIds := []string{}
	if openAll {
		sgID, e := ensureOpenAllSG(ctx, cli, region)
		if e != nil {
			fmt.Println("❌ 创建/配置安全组失败：", e)
			return
		}
		sgIds = []string{sgID}
		fmt.Println("✅ 将使用安全组：", sgID)
	} else {
		fmt.Println("（未选择全开端口，将使用默认安全组/默认规则）")
	}

	fmt.Println("\n🚀 RunInstances...")
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
		runIn.UserData = aws.String(userDataB64) // EC2 必须 base64
	}

	out, err := cli.RunInstances(ctx, runIn)
	if err != nil {
		fmt.Println("❌ RunInstances 失败：", err)
		fmt.Println("提示：AMI 不在该 region 或缺权限 ec2:RunInstances")
		return
	}
	if len(out.Instances) == 0 || out.Instances[0].InstanceId == nil {
		fmt.Println("❌ 创建失败：未返回实例 ID")
		return
	}
	id := *out.Instances[0].InstanceId
	fmt.Println("✅ 已创建实例：", id)

	if name != "" {
		_, _ = cli.CreateTags(ctx, &ec2.CreateTagsInput{
			Resources: []string{id},
			Tags:      []ec2t.Tag{{Key: aws.String("Name"), Value: aws.String(name)}},
		})
	}

	fmt.Println("⏳ 等待 running（最多 ~10 分钟）...")
	waiter := ec2.NewInstanceRunningWaiter(cli)
	_ = waiter.Wait(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}}, 10*time.Minute)

	desc, _ := cli.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if len(desc.Reservations) > 0 && len(desc.Reservations[0].Instances) > 0 {
		ins := desc.Reservations[0].Instances[0]
		pub := ""
		if ins.PublicIpAddress != nil {
			pub = *ins.PublicIpAddress
		}
		fmt.Printf("📡 %s  state=%s  public_ip=%s  region=%s\n", id, string(ins.State.Name), pub, region)
	}
}

//
// -------------------- Main Menu --------------------
//

func main() {
	ctx := context.Background()

	fmt.Println("=== AWS 管理工具（Go SDK / 运行后输入AKSK）===")
	fmt.Println("功能：EC2 建/管 + Lightsail 建/管\n")

	ak := input("AWS Access Key ID: ", "")
	sk := inputSecret("AWS Secret Access Key: ")
	token := inputSecret("AWS Session Token（可选，直接回车跳过）: ")
	if ak == "" || sk == "" {
		fmt.Println("❌ AK/SK 不能为空")
		return
	}

	bootstrap := input("引导 Region（用于校验凭证 & 拉区域列表）[us-east-1]: ", "us-east-1")
	creds := credentials.NewStaticCredentialsProvider(ak, sk, token)

	fmt.Println("\n🔍 校验凭证...")
	if err := stsCheck(ctx, bootstrap, creds); err != nil {
		fmt.Println("❌ 凭证校验失败：", err)
		fmt.Println("可能原因：AK/SK 错、权限不足、或时间不同步（临时凭证）")
		return
	}
	fmt.Println("✅ 凭证有效")

	fmt.Println("\n🌍 获取 EC2 Regions...")
	ec2Regions, err := getEC2Regions(ctx, bootstrap, creds)
	if err != nil {
		fmt.Println("⚠️ 获取 EC2 Regions 失败：", err)
		ec2Regions = []string{"us-east-1"}
	} else {
		fmt.Printf("✅ EC2 Regions: %d\n", len(ec2Regions))
	}

	fmt.Println("\n🌍 获取 Lightsail Regions...")
	lsRegions, err := getLightsailRegions(ctx, bootstrap, creds)
	if err != nil {
		fmt.Println("⚠️ 获取 Lightsail Regions 失败：", err)
		lsRegions = []string{"us-east-1"}
	} else {
		fmt.Printf("✅ Lightsail Regions: %d\n", len(lsRegions))
	}

	for {
		fmt.Println("\n================ 主菜单 ================")
		fmt.Println("1) EC2：建实例（可选全开端口 + 可选 user-data）")
		fmt.Println("2) EC2：控制实例（扫描所有 region）")
		fmt.Println("3) Lightsail：建光帆（可选全开端口 + 可选 user-data）")
		fmt.Println("4) Lightsail：控制光帆（扫描所有 region）")
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
