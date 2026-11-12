package feishu

import (
 "bytes"
 "encoding/binary"
 "errors"
 "strconv"
 "time"
 "github.com/Icatme/wechatbot-go/channels/internal/wire"
)

// These protocol field numbers are implemented locally, not generated/copied
// from a platform SDK. Unknown protobuf fields are preserved on ACK frames.
type frame struct{Seq,Log,Service,Method uint64;Headers map[string]string;Payload,Extra []byte}
type field struct{number uint64;typ byte;integer uint64;data,raw []byte}
func fields(b []byte)([]field,error){
 if len(b)>wire.MaxBody{return nil,errors.New("protobuf frame too large")};var out []field
 for len(b)>0{
  start:=b;tag,n:=binary.Uvarint(b);if n<=0||tag>>3==0{return nil,errors.New("invalid protobuf tag")};b=b[n:];f:=field{number:tag>>3,typ:byte(tag&7)}
  switch f.typ{
  case 0:v,n:=binary.Uvarint(b);if n<=0{return nil,errors.New("invalid protobuf integer")};f.integer=v;b=b[n:]
  case 2:v,n:=binary.Uvarint(b);if n<=0||v>uint64(len(b)-n){return nil,errors.New("invalid protobuf length")};b=b[n:];f.data=b[:int(v)];b=b[int(v):]
  case 1:if len(b)<8{return nil,errors.New("short fixed64")};b=b[8:]
  case 5:if len(b)<4{return nil,errors.New("short fixed32")};b=b[4:]
  default:return nil,errors.New("unsupported protobuf wire type")
  }
  f.raw=start[:len(start)-len(b)];out=append(out,f);if len(out)>4096{return nil,errors.New("too many protobuf fields")}
 };return out,nil
}
func decodeFrame(b []byte)(frame,error){
 f:=frame{Headers:map[string]string{}};fs,err:=fields(b);if err!=nil{return f,err}
 for _,x:=range fs{switch x.number{
 case 1,2,3,4:
  if x.typ!=0{return f,errors.New("invalid frame integer type")};switch x.number{case 1:f.Seq=x.integer;case 2:f.Log=x.integer;case 3:f.Service=x.integer;case 4:f.Method=x.integer}
 case 5:
  if x.typ!=2{return f,errors.New("invalid header type")};hs,err:=fields(x.data);if err!=nil{return f,err};var k,v string
  for _,h:=range hs{if h.typ==2{if h.number==1{k=string(h.data)};if h.number==2{v=string(h.data)}}};if k!=""{f.Headers[k]=v}
 case 8:if x.typ!=2{return f,errors.New("invalid payload type")};f.Payload=append([]byte(nil),x.data...)
 default:f.Extra=append(f.Extra,x.raw...)
 }};return f,nil
}
func varint(b []byte,n,v uint64)[]byte{b=binary.AppendUvarint(b,n<<3);return binary.AppendUvarint(b,v)}
func blob(b []byte,n uint64,p []byte)[]byte{b=binary.AppendUvarint(b,n<<3|2);b=binary.AppendUvarint(b,uint64(len(p)));return append(b,p...)}
func(f frame)encode()[]byte{
 var b []byte;b=varint(b,1,f.Seq);b=varint(b,2,f.Log);b=varint(b,3,f.Service);b=varint(b,4,f.Method)
 for k,v:=range f.Headers{h:=blob(nil,1,[]byte(k));h=blob(h,2,[]byte(v));b=blob(b,5,h)};b=append(b,f.Extra...);return blob(b,8,f.Payload)
}
type fragment struct{parts map[int][]byte;count,bytes int;expires time.Time}
type assembler struct{items map[string]*fragment;bytes int}
func(a *assembler)combine(f frame)([]byte,bool,error){
 count,index:=1,0;var err error
 if v:=f.Headers["sum"];v!=""{count,err=strconv.Atoi(v);if err!=nil{return nil,false,err}}
 if v:=f.Headers["seq"];v!=""{index,err=strconv.Atoi(v);if err!=nil{return nil,false,err}}
 if count<1||count>128||index<0||index>=count{return nil,false,errors.New("invalid fragment bounds")};if count==1{return f.Payload,true,nil}
 id:=f.Headers["message_id"];if id==""{return nil,false,errors.New("fragment has no message_id")};if a.items==nil{a.items=map[string]*fragment{}}
 now:=time.Now();for k,v:=range a.items{if !now.Before(v.expires){a.bytes-=v.bytes;delete(a.items,k)}}
 p:=a.items[id];if p==nil{if len(a.items)>=32{return nil,false,errors.New("fragment capacity exhausted")};p=&fragment{parts:map[int][]byte{},count:count,expires:now.Add(30*time.Second)};a.items[id]=p}
 if p.count!=count{return nil,false,errors.New("fragment count changed")}
 if old,ok:=p.parts[index];ok{if !bytes.Equal(old,f.Payload){return nil,false,errors.New("conflicting fragment")}}else{
  if p.bytes+len(f.Payload)>wire.MaxBody||a.bytes+len(f.Payload)>8<<20{return nil,false,errors.New("fragment payload too large")}
  p.parts[index]=append([]byte(nil),f.Payload...);p.bytes+=len(f.Payload);a.bytes+=len(f.Payload)
 }
 if len(p.parts)!=count{return nil,false,nil};out:=make([]byte,0,p.bytes);for i:=0;i<count;i++{out=append(out,p.parts[i]...)};a.bytes-=p.bytes;delete(a.items,id);return out,true,nil
}
