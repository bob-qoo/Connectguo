import 'dart:convert';

import 'package:duanju_app/core_bridge.dart';
import 'package:duanju_app/local_profiles.dart';
import 'package:duanju_app/local_store.dart';
import 'package:duanju_app/main.dart';
import 'package:duanju_app/models.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:shared_preferences/shared_preferences.dart';

import 'fixtures.dart';

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();
  const drama = Drama(id: 'hongguo:1', source: 'hongguo', title: '合成短剧');
  const denied = Drama(id: 'huangdou:2', source: 'huangdou', title: '受限合成短剧');

  test(
    'Connectguo unrestricted: no lock, all sources and downloads allowed',
    () async {
      SharedPreferences.setMockInitialValues({});
      final prefs = await SharedPreferences.getInstance();
      final store = LocalStore(prefs);
      await store.toggleFavorite(drama);
      await store.saveProfile(
        id: 'default',
        name: '管理员',
        sources: [],
        download: true,
        pin: 'abcdef12',
      );
      await store.saveProfile(
        name: '访客',
        sources: ['hongguo'],
        download: false,
      );
      final visitor = store.profiles.firstWhere((p) => !p.admin);
      await store.switchProfile(visitor.id);
      expect(store.favorites, isEmpty);
      // 站源与下载不再受用户档案限制
      expect(
        store.sources.map((s) => s.id),
        SourceSite.values.map((s) => s.id),
      );
      expect(store.canDownload, isTrue);
      await store.toggleFavorite(denied);
      expect(store.favorites.single.id, denied.id);
      await store.toggleFavorite(drama);
      await store.clearHistory();
      await expectLater(
        store.switchProfile('default', pin: 'wrong'),
        throwsStateError,
      );
      expect(store.profile.id, visitor.id);
      await store.switchProfile('default', pin: 'abcdef12');
      expect(store.favorites.single.id, drama.id);
      expect(prefs.getString('profiles'), isNot(contains('abcdef12')));
      final restarted = LocalStore(prefs);
      expect(restarted.locked, isFalse);
      expect(
        restarted.sources.map((s) => s.id),
        SourceSite.values.map((s) => s.id),
      );
      await restarted.switchProfile('default', pin: 'abcdef12');
      expect(restarted.canDownload, isTrue);
      store.dispose();
      restarted.dispose();
    },
  );

  test('backup roundtrip validates before replacing account data', () async {
    SharedPreferences.setMockInitialValues({
      'plugin_secret': 'must-stay-local',
    });
    final store = LocalStore(await SharedPreferences.getInstance());
    await store.toggleFavorite(drama);
    await store.saveWatch(
      WatchEntry(
        drama: drama,
        episode: 2,
        position: 11,
        duration: 60,
        updatedAt: DateTime.now(),
      ),
    );
    final backup = await store.exportBackup();
    expect(backup, isNot(contains('must-stay-local')));
    await store.toggleFavorite(drama);
    await store.importBackup(backup);
    expect(store.favorites.single.id, drama.id);
    expect(store.watched(drama.id)?.position, 11);
    final invalid = jsonDecode(backup) as Map<String, dynamic>;
    (invalid['profiles'] as List).add({
      'id': '../escape',
      'name': 'bad',
      'sources': [],
    });
    await expectLater(
      store.importBackup(jsonEncode(invalid)),
      throwsA(anything),
    );
    expect(store.favorites.single.id, drama.id);
    expect(store.preferences.getString('plugin_secret'), 'must-stay-local');
    store.dispose();
  });

  test(
    'Connectguo unrestricted: permission gates allow every source and download',
    () async {
      SharedPreferences.setMockInitialValues({
        'profiles': jsonEncode([
          const LocalProfile(
            id: 'default',
            name: '管理员',
            admin: true,
            salt: '11111111111111111111111111111111',
            pinHash:
                'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
          ).toJson(),
          const LocalProfile(
            id: 'viewer',
            name: '只看红果',
            sources: ['hongguo'],
            download: false,
          ).toJson(),
        ]),
        'activeProfile': 'viewer',
      });
      final store = LocalStore(await SharedPreferences.getInstance());
      final repository = NativeRepository()..access = store;
      expect(store.locked, isFalse);
      expect(store.canDownload, isTrue);
      expect(repository.supportsDownloads, isTrue);
      for (final source in SourceSite.values.map((s) => s.id)) {
        expect(store.allowsSource(source), isTrue);
      }
      expect(
        store.sources.map((s) => s.id),
        SourceSite.values.map((s) => s.id),
      );
      store.dispose();
    },
  );

  testWidgets('Connectguo unrestricted home shows downloads and all sources', (
    tester,
  ) async {
    SharedPreferences.setMockInitialValues({
      'profiles': jsonEncode([
        const LocalProfile(
          id: 'default',
          name: '管理员',
          admin: true,
          salt: '11111111111111111111111111111111',
          pinHash:
              'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
        ).toJson(),
        const LocalProfile(
          id: 'viewer',
          name: '只看红果',
          sources: ['hongguo'],
          download: false,
        ).toJson(),
      ]),
      'activeProfile': 'viewer',
    });
    final store = LocalStore(await SharedPreferences.getInstance());
    await tester.pumpWidget(
      DuanjuApp(repository: FixtureRepository(), store: store),
    );
    await tester.pumpAndSettle();
    expect(find.text('下载'), findsWidgets);
    expect(find.text('黄豆'), findsWidgets);
    expect(find.text('黄果 AI'), findsWidgets);
    expect(find.text('红果'), findsWidgets);
    await tester.pumpWidget(const SizedBox.shrink());
    store.dispose();
  });
}
