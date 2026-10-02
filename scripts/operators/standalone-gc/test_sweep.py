import unittest
from sweep import candidate_number, duration, eligible, Settings, load_settings
PROJECT = '00000000-0000-0000-0000-000000000001'

class SweepTests(unittest.TestCase):
    def test_both_name_orders(self):
        for name, number in [('mdt792-wt',792),('wt-mdt890',890),('wt-mdt-1020',1020)]:
            self.assertEqual(candidate_number(name),number)
        for name in ['mono','mdt123','wt-foo','mdt1-mdt2-wt']:
            self.assertIsNone(candidate_number(name))
    def test_number_only_requires_explicit_workspace_root(self):
        self.assertIsNone(candidate_number('wt-937'))
        self.assertEqual(candidate_number('wt-937', True),937)
        self.assertIsNone(candidate_number('wt-937-extra', True))
    def test_terminal_age_and_identity(self):
        card={'project_id':PROJECT,'number':792,'status':'done','updated_at':'1970-01-01T00:00:00Z'}
        self.assertTrue(eligible(card,792,1801,1800,PROJECT))
        self.assertFalse(eligible(card,792,1799,1800,PROJECT))
        for status in ['todo','in_progress','in_review','blocked']:
            self.assertFalse(eligible(dict(card,status=status),792,10000,1800,PROJECT))
        self.assertFalse(eligible(dict(card,project_id='different'),792,10000,1800,PROJECT))
        self.assertFalse(eligible(card,890,10000,1800,PROJECT))
    def test_existing_config_duration(self):
        self.assertEqual(duration('30m'),1800)
        self.assertEqual(duration('1h'),3600)
        with self.assertRaises(ValueError):duration('bad')


# Exercise actual Git worktrees; the Multica lookup is injected, never a user CLI.
import contextlib
import io
import json
from pathlib import Path
import subprocess
import tempfile
from unittest.mock import patch
import sweep as module

class WorktreeSweepTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.base = Path(self.tmp.name).resolve()
        self.addCleanup(self.tmp.cleanup)
        (self.base / '.multica').mkdir()
        (self.base / '.multica/config.json').write_text('{"gc_ttl":"30m"}')
        self.origin = self.base / 'origin.git'
        self.repo = self.base / 'repo'
        self.run_git('init', '--bare', str(self.origin))
        self.run_git('init', '-b', 'main', str(self.repo))
        self.run_git('-C', str(self.repo), 'config', 'user.email', 'test@example.invalid')
        self.run_git('-C', str(self.repo), 'config', 'user.name', 'Test')
        (self.repo / 'tracked').write_text('initial\n')
        (self.repo / '.gitignore').write_text('node_modules/\n')
        self.run_git('-C', str(self.repo), 'add', '.')
        self.run_git('-C', str(self.repo), 'commit', '-m', 'base')
        self.run_git('-C', str(self.repo), 'remote', 'add', 'origin', str(self.origin))
        self.run_git('-C', str(self.repo), 'push', 'origin', 'main')
        self.wt = self.base / 'mdt792-wt'
        self.run_git('-C', str(self.repo), 'worktree', 'add', '-b', 'task', str(self.wt))
        self.settings = Settings(PROJECT, 'MDT', ((self.base, False),), self.base / '.multica/config.json', self.base / '.multica/gc.lock')
        self.card = {'project_id': PROJECT, 'number': 792, 'status': 'done', 'updated_at': '1970-01-01T00:00:00Z'}
        self.real_command = module.command
    def run_git(self, *args):
        return subprocess.run(['git', *args],check=True,capture_output=True,text=True).stdout
    def execute(self, apply=False, active=False):
        def command(args):
            if args[0] == 'multica': return json.dumps(self.card)
            return self.real_command(args)
        with patch.object(module,'command',side_effect=command), patch.object(module,'active',return_value=active), contextlib.redirect_stdout(io.StringIO()):
            return module.sweep(apply,self.settings)[0]['result']
    def test_dry_run_keeps_clean_worktree(self):
        self.assertEqual(self.execute(),'would remove')
        self.assertTrue(self.wt.exists())
    def test_apply_removes_clean_registered_worktree(self):
        (self.wt / 'node_modules').mkdir()
        (self.wt / 'node_modules/rebuildable').write_text('cache')
        self.assertEqual(self.execute(True),'removed')
        self.assertFalse(self.wt.exists())
        self.assertNotIn(str(self.wt),self.run_git('-C',str(self.repo),'worktree','list','--porcelain'))
    def test_untracked_work_is_kept(self):
        (self.wt / 'unsaved').write_text('work')
        self.assertEqual(self.execute(True),'kept: local changes')
        self.assertTrue((self.wt / 'unsaved').exists())
    def test_unmerged_commit_is_kept(self):
        (self.wt / 'tracked').write_text('unmerged\n')
        self.run_git('-C',str(self.wt),'commit','-am','unmerged')
        self.assertEqual(self.execute(True),'kept: commits not represented on configured base')
        self.assertTrue(self.wt.exists())
    def test_active_process_is_kept(self):
        self.assertEqual(self.execute(True,active=True),'kept: active process')
    def test_open_card_is_kept(self):
        self.card['status']='in_review'
        self.assertEqual(self.execute(True),'kept: card not terminal past workspace TTL')
    def test_symlink_candidate_is_not_followed(self):
        self.wt.rename(self.base / 'original')
        self.wt.symlink_to(self.base / 'original', target_is_directory=True)
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(module.sweep(True,self.settings),[])
        self.assertTrue((self.base / 'original/tracked').exists())

class ConfigurationTests(unittest.TestCase):
    def test_explicit_roots_project_and_prefix(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp).resolve()
            cfg = root / 'sweep.json'
            cfg.write_text(json.dumps({'project_id':PROJECT,'card_prefix':'OPS',
                'roots':[{'path':str(root),'allow_number_only':True}],
                'daemon_config':str(root/'daemon.json'),'lock_file':str(root/'lock')}))
            settings=load_settings(cfg)
            self.assertEqual(settings.project_id,PROJECT)
            self.assertEqual(settings.prefix,'OPS')
            self.assertEqual(settings.roots,((root,True),))
            self.assertEqual(candidate_number('wt-ops-12',prefix=settings.prefix),12)
            self.assertIsNone(candidate_number('wt-mdt-12',prefix=settings.prefix))
    def test_missing_project_refused(self):
        with tempfile.TemporaryDirectory() as temp:
            cfg=Path(temp)/'config.json';cfg.write_text('{}')
            with self.assertRaises(KeyError):load_settings(cfg)
    def test_linked_root_refused(self):
        with tempfile.TemporaryDirectory() as temp:
            root=Path(temp).resolve();(root/'link').symlink_to(root,target_is_directory=True)
            cfg=root/'config.json';cfg.write_text(json.dumps({'project_id':PROJECT,'card_prefix':'OPS',
                'roots':[{'path':str(root/'link')}],'daemon_config':str(root/'daemon.json'),'lock_file':str(root/'lock')}))
            with self.assertRaises(ValueError):load_settings(cfg)

if __name__ == "__main__":
    unittest.main()
