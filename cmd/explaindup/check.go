package main
import (
  "bufio","context","fmt","os","strings","time"
  "github.com/jackc/pgx/v5/pgxpool"
)
func main(){
  f,_:=os.Open(".env"); sc:=bufio.NewScanner(f); var url string
  for sc.Scan(){ if v,ok:=strings.CutPrefix(strings.TrimSpace(sc.Text()),"DATABASE_URL="); ok { url=strings.TrimSpace(v) } }
  f.Close()
  ctx,_:=context.WithTimeout(context.Background(),30*time.Second)
  pool,_:=pgxpool.New(ctx,url); defer pool.Close()
  rows,_:=pool.Query(ctx, `SELECT indexname, indexdef FROM pg_indexes WHERE tablename='Lead' AND indexname IN ('Lead_leadEmail_lower_idx','Lead_phone_digits_idx')`)
  for rows.Next(){ var n,d string; rows.Scan(&n,&d); fmt.Println(n); fmt.Println(" ",d) }
  rows.Close()
  // create if missing
  _,err:=pool.Exec(ctx, `CREATE INDEX IF NOT EXISTS "Lead_leadEmail_lower_idx" ON "Lead" (lower("leadEmail")) WHERE "leadEmail" IS NOT NULL AND BTRIM("leadEmail") <> ''`)
  fmt.Println("create email idx:", err)
}
