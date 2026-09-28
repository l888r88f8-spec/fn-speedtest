package main

import (
	"strings"
	"testing"
)

func TestParseSpiritNetCatalogFiltersFalseCNRows(t *testing.T){
	data := "id,country_code,country,city,ip,host,port,supplier\n"+
		"5396,CN,China,Suzhou,1.1.1.1,node.example,8080,China Telecom JiangSu 5G\n"+
		"73010,CN,Pakistan,Helan,1.1.1.2,bad.example,8080,Arslan Telecom\n"
	nodes,err:=parseSpiritNetCatalog([]byte(data))
	if err!=nil||len(nodes)!=1{t.Fatalf("nodes=%+v err=%v",nodes,err)}
	if nodes[0].ID!="5396"||nodes[0].Province!="江苏"||nodes[0].Carrier!="中国电信"||!strings.Contains(nodes[0].URL,"/speedtest/upload.php"){t.Fatalf("node=%+v",nodes[0])}
}

func TestParseSukkaNetCatalogKeepsMainlandOnly(t *testing.T){
	data:=`[{"url":"http://cn.example:8080/speedtest/upload.php","name":"Nanjing","country":"China","cc":"CN","sponsor":"China Unicom","id":"100","host":"cn.example:8080"},{"url":"http://jp.example/upload.php","name":"Tokyo","country":"Japan","cc":"JP","sponsor":"ISP","id":"200","host":"jp.example"}]`
	nodes,err:=parseSukkaNetCatalog([]byte(data))
	if err!=nil||len(nodes)!=1{t.Fatalf("nodes=%+v err=%v",nodes,err)}
	if nodes[0].ID!="100"||nodes[0].Carrier!="中国联通"||nodes[0].Province!="江苏"{t.Fatalf("node=%+v",nodes[0])}
}

func TestCustomOoklaResultUsesSpeedtestNetEngine(t *testing.T){
	target,err:=customOoklaTarget(vpsDanceOoklaNodes[0],networkIdentity{})
	if err!=nil{t.Fatal(err)}
	result:=speedtestCNResult(target,10,1,100,50)
	if result.Engine!="Speedtest.net"{t.Fatalf("engine=%q",result.Engine)}
}
