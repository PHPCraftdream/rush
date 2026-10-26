import importlib.util
import json
import pathlib
import tempfile
import unittest
from unittest.mock import patch

FILE=pathlib.Path(__file__).with_name("ci_gate_decide.py")
spec=importlib.util.spec_from_file_location("ci_gate_decide",FILE); gate=importlib.util.module_from_spec(spec); spec.loader.exec_module(gate)
SHA="a"*40
MODULE=gate.MODULE
GOOD={"plan":"success","checks":"success"}
FULL_PACKAGES=[MODULE+"/internal/agent",MODULE+"/internal/hooks",MODULE+"/internal/db"]

class DecideTests(unittest.TestCase):
    def args(self,**kw):
        d=dict(event_name="pull_request",base_ref="feature",labels=[],mode="tier1",needed_job_results=dict(GOOD),validated_sha=SHA,current_sha=SHA,validated_base_sha=SHA,current_base_sha=SHA,expected_jobs=list(GOOD)); d.update(kw); return d
    def test_transition_table(self):
        cases=[("task tier1",{},"pass"),("main labeled full",dict(base_ref="main",labels=["full-ci"],mode="full"),"pass"),("main unlabeled",dict(base_ref="main"),"fail"),("main labeled tier1",dict(base_ref="main",labels=["full-ci"]),"fail"),("push main full",dict(event_name="push",base_ref="main",mode="full"),"pass"),("push main tier1",dict(event_name="push",base_ref="main"),"fail"),("stale head",dict(current_sha="b"*40),"fail"),("stale base",dict(current_base_sha="b"*40),"fail"),("empty expected",dict(expected_jobs=[]),"fail"),("bad mode",dict(mode="invalid"),"fail"),("needed job skipped",dict(needed_job_results={"plan":"success","checks":"skipped"}),"fail"),("draft labeled full",dict(base_ref="main",labels=["full-ci"],mode="full",draft=True),"pass"),("draft without label",dict(base_ref="main",draft=True),"fail"),("unrelated label event",dict(base_ref="main",event_action="labeled",event_label="bug",expected_jobs=[]),"pass")]
        for name,changes,want in cases:
            with self.subTest(name=name): self.assertEqual(gate.decide(**self.args(**changes))["verdict"],want)
    def test_exact_red_reason_and_stale_revision(self):
        self.assertEqual(gate.decide(**self.args(base_ref="main"))["reason"],gate.NO_FULL)
        self.assertIn("stale revision",gate.decide(**self.args(current_sha="b"*40))["reason"])
    def test_unrelated_label_ignored_even_with_invalid_plan(self):
        r=gate.decide("pull_request","main",[],"bad",{},None,None,expected_jobs=[],plan_valid=False,event_action="unlabeled",event_label="bug")
        self.assertEqual(r["verdict"],"pass"); self.assertIn("Ignored unrelated",r["reason"])

class JobPlanTests(unittest.TestCase):
    def test_exact_full_leaf_inventory(self):
        p={"mode":"full"}; outputs={k:True for k in ("ubuntu_needed","macos_needed","windows_needed","agent_selected","race_needed","web_changed","security_full","security_actions","security_deps")}
        jobs,not_run=gate.expected_jobs(p,outputs)
        expected=["plan","checks","web","tests / test-ubuntu","tests / agent-ubuntu","tests / race-ubuntu","tests / test-native (macos-latest)","tests / windows-full-shards / plan-shards",*[f"tests / windows-full-shards / windows-shard ({x})" for x in "ABCD"],"tests / windows-full-shards / windows-agent","tests / windows-full-shards / shards-summary","security / codeql-go","security / codeql-actions","security / govulncheck","security / grype"]
        self.assertEqual(jobs,expected); self.assertFalse(any("windows-latest" in x["job"] for x in not_run))
    def test_native_required_prepare_cli_exports_both_lanes(self):
        plan={"schema_version":1,"mode":"tier1","per_os":{"linux":[],"darwin":[],"windows":[]},"race_packages":[],"not_run":[],"native_required":{"required":True},"head":SHA,"base":SHA}
        with tempfile.TemporaryDirectory() as td:
            pp=pathlib.Path(td)/"p.json"; out=pathlib.Path(td)/"out.json"; pp.write_text(json.dumps(plan)); args=type("A",(),{"plan":str(pp),"graph_dir":td,"force_full":False,"policy":str(pathlib.Path(td)/"policy"),"base":SHA,"head":SHA,"output":str(out)})()
            with patch.object(gate.subprocess,"run",return_value=type("R",(),{"stdout":""})()),patch.dict("os.environ",{},clear=True): gate.prepare_cli(args)
            result=json.loads(out.read_text())["expected_outputs"]
            self.assertEqual(result["mode"],"tier1"); self.assertTrue(result["macos_needed"]); self.assertTrue(result["windows_needed"])
    def assignment(self,packages=None):
        packages=FULL_PACKAGES if packages is None else packages
        shards={x:{"groups":[]} for x in "ABCD"}; shards["A"]["groups"]=[{"name":"a","packages":packages[1:]}]
        return {"module":MODULE,"agent":[MODULE+"/internal/agent"],"shards":shards,"unassigned":[],"counts":{"agent":1,"A":len(packages)-1,"B":0,"C":0,"D":0,"total":len(packages)}}
    def test_assignment_requires_exact_inventory_and_consistent_counts(self):
        a=self.assignment(); self.assertTrue(gate.validate_assignment(a,FULL_PACKAGES))
        cases=[]
        missing=self.assignment(FULL_PACKAGES[:-1]); cases.append(("missing package",missing))
        duplicate=self.assignment(); duplicate["shards"]["B"]["groups"]=[{"packages":[FULL_PACKAGES[1]]}]; duplicate["counts"]["B"]=1; duplicate["counts"]["total"]+=1; cases.append(("duplicate package",duplicate))
        empty=self.assignment([]); cases.append(("empty fake",empty))
        wrong_total=self.assignment(); wrong_total["counts"]["total"]+=1; cases.append(("wrong total",wrong_total))
        wrong_agent=self.assignment(); wrong_agent["agent"]=[MODULE+"/internal/other"]; cases.append(("wrong agent",wrong_agent))
        for name,bad in cases:
            with self.subTest(name=name),self.assertRaises(ValueError): gate.validate_assignment(bad,FULL_PACKAGES)

class AdapterTests(unittest.TestCase):
    def assignment(self):
        packages=FULL_PACKAGES
        shards={x:{"groups":[]} for x in "ABCD"}; shards["A"]["groups"]=[{"name":"a","packages":packages[1:]}]
        return {"module":MODULE,"agent":[MODULE+"/internal/agent"],"shards":shards,"unassigned":[],"counts":{"agent":1,"A":len(packages)-1,"B":0,"C":0,"D":0,"total":len(packages)}}
    def setup_contract(self,td,base_ref="feature"):
        outputs={"mode":"tier1","ubuntu_needed":False,"macos_needed":False,"windows_needed":False,"linux_packages":"","darwin_packages":"","windows_packages":"","race_packages":"","race_needed":False,"agent_selected":False,"web_changed":False,"security_full":False,"security_actions":False,"security_deps":False,"heavy_allowed":True,"native_required":False,"validated_head":SHA,"validated_base":SHA}
        plan={"schema_version":1,"mode":"tier1","head":SHA,"base":SHA,"not_run":[{"os":"linux","package_or_suite":"docs","reason":"not selected"}],"per_os":{"linux":[],"darwin":[],"windows":[]},"race_packages":[],"native_required":{"required":False},"expected_outputs":outputs}
        pp=pathlib.Path(td)/"plan.json"; pp.write_text(json.dumps(plan)); out=pathlib.Path(td)/"out.json"
        args=type("A",(),{"plan":str(pp),"output":str(out),"base_ref":base_ref,"assignment":None})()
        env={"GITHUB_EVENT_NAME":"pull_request","GITHUB_EVENT_ACTION":"synchronize","GITHUB_EVENT_LABEL":"","GITHUB_BASE_REF":base_ref,"GITHUB_REPOSITORY":"org/repo","GITHUB_RUN_ID":"9","GITHUB_RUN_ATTEMPT":"2","PR_NUMBER":"12","PLAN_OUTPUTS_JSON":json.dumps(outputs),"NEEDS_JSON":json.dumps({"plan":"success","checks":"success","tests":"success","web":"skipped","security":"skipped"}),"VALIDATED_HEAD":SHA,"VALIDATED_BASE":SHA,"PR_DRAFT":"false"}
        return plan,outputs,args,env
    def jobs_api(self,plan=None,outputs=None):
        jobs=[{"name":"plan","conclusion":"success","run_attempt":2},{"name":"checks","conclusion":"success","run_attempt":2}]
        if outputs and outputs.get("web_changed"): jobs.append({"name":"web","conclusion":"success","run_attempt":2})
        return [{"jobs":jobs}]
    def pr(self,labels=(),head=SHA,base=SHA,ref="feature"):
        return {"head":{"sha":head},"base":{"sha":base,"ref":ref},"labels":[{"name":x} for x in labels]}
    def test_mock_api_passes_current_attempt_and_notrun(self):
        with tempfile.TemporaryDirectory() as td:
            plan,outputs,args,env=self.setup_contract(td); responses=[self.pr(),self.jobs_api()]
            with patch.dict("os.environ",env,clear=True),patch.object(gate,"run_json",side_effect=responses) as api:
                result=gate.gate_cli(args)
            self.assertEqual(result["verdict"],"pass",result["reason"]); self.assertEqual(result["not_run"][0]["package_or_suite"],"docs"); self.assertEqual(api.call_count,2)
    def test_dependency_failure_still_fetches_live_pr_first(self):
        with tempfile.TemporaryDirectory() as td:
            _,_,args,env=self.setup_contract(td); env["NEEDS_JSON"]="{}"
            with patch.dict("os.environ",env,clear=True),patch.object(gate,"run_json",return_value=self.pr()) as api:
                result=gate.gate_cli(args)
            self.assertEqual(api.call_count,1); self.assertIn("dependency did not succeed",result["reason"])
    def test_live_label_removed_minimal_plan_summary_lists_skips_and_needs(self):
        with tempfile.TemporaryDirectory() as td:
            _,_,args,env=self.setup_contract(td,"main"); env["GITHUB_BASE_REF"]="main"; env["PR_BASE_REF"]="main"; env["GITHUB_EVENT_ACTION"]="unlabeled"; env["GITHUB_EVENT_LABEL"]="full-ci"; env["NEEDS_JSON"]=json.dumps({"plan":"skipped","checks":"success","tests":"skipped","web":"skipped","security":"skipped"}); summary=pathlib.Path(td)/"summary.md"; env["GITHUB_STEP_SUMMARY"]=str(summary)
            from unittest.mock import patch
            with patch.dict("os.environ",env,clear=True):
                gate.minimal_cli(type("A",(),{"base":SHA,"head":SHA,"output":args.plan})())
            with patch.dict("os.environ",env,clear=True),patch.object(gate,"run_json",return_value=self.pr(labels=(),ref="main")):
                result=gate.gate_cli(args)
            text=summary.read_text()
            self.assertEqual(result["reason"],gate.NO_FULL); self.assertEqual(result["jobs"]["plan"],"skipped"); self.assertEqual(result["jobs"]["checks"],"success")
            self.assertIn("heavy suites",text); self.assertIn("tests / test-ubuntu",text); self.assertIn("security / codeql-go",text); self.assertIn("web",text)
    def test_live_label_removed_with_plan_missing_returns_no_full(self):
        with tempfile.TemporaryDirectory() as td:
            _,_,args,env=self.setup_contract(td,"main"); args.plan=str(pathlib.Path(td)/"absent.json"); env["GITHUB_BASE_REF"]="main"; env["PR_BASE_REF"]="main"; env["GITHUB_EVENT_ACTION"]="unlabeled"; env["GITHUB_EVENT_LABEL"]="full-ci"
            with patch.dict("os.environ",env,clear=True),patch.object(gate,"run_json",return_value=self.pr(labels=(),ref="main")) as api:
                result=gate.gate_cli(args)
            self.assertEqual(result["reason"],gate.NO_FULL); self.assertEqual(api.call_count,1)
    def test_live_readded_label_with_tier1_mode_returns_no_full(self):
        with tempfile.TemporaryDirectory() as td:
            _,_,args,env=self.setup_contract(td,"main"); env["GITHUB_BASE_REF"]="main"; env["PR_BASE_REF"]="main"; env["GITHUB_EVENT_ACTION"]="unlabeled"; env["GITHUB_EVENT_LABEL"]="full-ci"
            with patch.dict("os.environ",env,clear=True),patch.object(gate,"run_json",side_effect=[self.pr(labels=["full-ci"],head="b"*40,ref="main"),self.jobs_api()]):
                result=gate.gate_cli(args)
            self.assertEqual(result["reason"],gate.NO_FULL); self.assertEqual(result["verdict"],"fail")
    def test_stale_sha_fails_after_live_state_query(self):
        with tempfile.TemporaryDirectory() as td:
            _,_,args,env=self.setup_contract(td); env["VALIDATED_HEAD"]=SHA
            with patch.dict("os.environ",env,clear=True),patch.object(gate,"run_json",side_effect=[self.pr(head="b"*40),self.jobs_api()]): result=gate.gate_cli(args)
            self.assertEqual(result["verdict"],"fail"); self.assertIn("stale revision",result["reason"])
    def test_unrelated_unlabeled_event_uses_no_api(self):
        with tempfile.TemporaryDirectory() as td:
            _,_,args,env=self.setup_contract(td,"main"); env["GITHUB_EVENT_ACTION"]="unlabeled"; env["GITHUB_EVENT_LABEL"]="bug"
            with patch.dict("os.environ",env,clear=True),patch.object(gate,"run_json") as api: result=gate.gate_cli(args)
            self.assertEqual(result["verdict"],"pass"); self.assertEqual(api.call_count,0)
    def test_missing_full_windows_assignment_fails_after_mocked_api(self):
        with tempfile.TemporaryDirectory() as td:
            plan,outputs,args,env=self.setup_contract(td); outputs.update(mode="full",ubuntu_needed=True,macos_needed=True,windows_needed=True,agent_selected=True,race_needed=True,web_changed=True,security_full=True,security_actions=True,security_deps=True,linux_packages=" ".join(FULL_PACKAGES),darwin_packages=" ".join(FULL_PACKAGES),windows_packages=" ".join(FULL_PACKAGES),race_packages=MODULE+"/internal/hooks",native_required=True); plan.update(mode="full",per_os={o:[{"package":p} for p in FULL_PACKAGES] for o in ("linux","darwin","windows")},race_packages=[MODULE+"/internal/hooks"]); plan["expected_outputs"]=outputs; pathlib.Path(args.plan).write_text(json.dumps(plan)); env["PLAN_OUTPUTS_JSON"]=json.dumps(outputs); env["NEEDS_JSON"]=json.dumps({"plan":"success","checks":"success","tests":"success","web":"success","security":"success"}); env["VALIDATED_HEAD"]=SHA; env["VALIDATED_BASE"]=SHA
            with patch.dict("os.environ",env,clear=True),patch.object(gate,"run_json",side_effect=[self.pr(),self.jobs_api()]):
                result=gate.gate_cli(args)
            self.assertEqual(result["verdict"],"fail"); self.assertIn("missing assignment",result["reason"])
    def test_full_assignment_pass_end_to_end(self):
        with tempfile.TemporaryDirectory() as td:
            plan,outputs,args,env=self.setup_contract(td); outputs.update(mode="full",ubuntu_needed=True,macos_needed=True,windows_needed=True,agent_selected=True,race_needed=True,web_changed=True,security_full=True,security_actions=True,security_deps=True,linux_packages=" ".join(FULL_PACKAGES),darwin_packages=" ".join(FULL_PACKAGES),windows_packages=" ".join(FULL_PACKAGES),race_packages=MODULE+"/internal/hooks",native_required=True); plan.update(mode="full",per_os={o:[{"package":p} for p in FULL_PACKAGES] for o in ("linux","darwin","windows")},race_packages=[MODULE+"/internal/hooks"]); plan["expected_outputs"]=outputs
            needed,_=gate.expected_jobs(plan,outputs); api_jobs=[{"name":n,"conclusion":"success","run_attempt":2} for n in needed]; assignment=pathlib.Path(td)/"assignment.json"; assignment.write_text(json.dumps(self.assignment())); args.assignment=str(assignment); pathlib.Path(args.plan).write_text(json.dumps(plan)); env["PLAN_OUTPUTS_JSON"]=json.dumps(outputs); env["NEEDS_JSON"]=json.dumps({"plan":"success","checks":"success","tests":"success","web":"success","security":"success"})
            with patch.dict("os.environ",env,clear=True),patch.object(gate,"run_json",side_effect=[self.pr(labels=["full-ci"],ref="main"),[{"jobs":api_jobs}]]): result=gate.gate_cli(args)
            self.assertEqual(result["verdict"],"pass",result["reason"])
    def test_output_drift_and_missing_plan_fail(self):
        with tempfile.TemporaryDirectory() as td:
            _,outputs,args,env=self.setup_contract(td); outputs["web_changed"]=True; env["PLAN_OUTPUTS_JSON"]=json.dumps(outputs)
            with patch.dict("os.environ",env,clear=True),patch.object(gate,"run_json",return_value=self.pr()): self.assertEqual(gate.gate_cli(args)["verdict"],"fail")
    def test_cancel_only_pr_associated_full_runs(self):
        env={"GITHUB_EVENT_NAME":"pull_request","GITHUB_EVENT_ACTION":"unlabeled","GITHUB_EVENT_LABEL":"full-ci","GITHUB_REPOSITORY":"o/r","PR_NUMBER":"4","GITHUB_RUN_ID":"10","CI_WORKFLOW_ID":"5"}
        runs={"workflow_runs":[{"id":10,"status":"in_progress","display_title":"ci [full]","workflow_id":5,"pull_requests":[{"number":4}]},{"id":11,"status":"in_progress","display_title":"ci [full]","workflow_id":5,"pull_requests":[]}]}
        with patch.dict("os.environ",env,clear=True),patch.object(gate,"run_json",side_effect=[{"labels":[]},runs]),patch.object(gate.subprocess,"run") as cancel:
            msg=gate.cancel_full_cli(); self.assertIn("WARNING",msg); cancel.assert_not_called()

if __name__=="__main__": unittest.main()
