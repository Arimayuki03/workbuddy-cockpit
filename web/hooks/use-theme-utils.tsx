import {useTheme} from 'next-themes';
import {SunIcon, MoonIcon} from 'lucide-react';
import {useCallback} from 'react';
import {t} from '@/lib/i18n';

const SystemTheme = {
  LIGHT: 'light',
  DARK: 'dark',
} as const;

const UserTheme = {
  ...SystemTheme,
  SYSTEM: 'system',
} as const;

type SystemTheme = (typeof SystemTheme)[keyof typeof SystemTheme];

type UserTheme = (typeof UserTheme)[keyof typeof UserTheme];

type ThemeSelectSpec<T> = {
  light: T;
  dark: T;
  system: T;
  default: T;
};

type SystemThemeSelectSpec<T> = {
  light: T;
  dark: T;
};

const getBaseRules = <T, >(light: T, dark: T) => ({
  light,
  dark,
});

const getSystemTheme = (): SystemTheme => {
  if (typeof window !== 'undefined') {
    return window.matchMedia(`(prefers-color-scheme: ${SystemTheme.DARK})`)
        .matches ?
      SystemTheme.DARK :
      SystemTheme.LIGHT;
  }
  return SystemTheme.LIGHT;
};

const selectSystem = <T, >(spec: SystemThemeSelectSpec<T>): T => {
  switch (getSystemTheme()) {
    case SystemTheme.LIGHT:
      return spec.light;
    case SystemTheme.DARK:
      return spec.dark;
  }
};

export function useThemeUtils() {
  const {theme, setTheme} = useTheme();

  const select = useCallback(
    <T, >(spec: ThemeSelectSpec<T>): T => {
      switch (theme) {
        case UserTheme.LIGHT:
          return spec.light;
        case UserTheme.DARK:
          return spec.dark;
        case UserTheme.SYSTEM:
          return spec.system;
        default:
          return spec.default;
      }
    },
    [theme],
  );

  // 显式三态循环：light → dark → system → light。
  // 用用户设置值（theme）直接推导下一态，不查系统偏好；
  // 'system' 态（含挂载前的 undefined 兜底）必然轮到 'light'。
  const toggle = useCallback(() => {
    const nextTheme =
      theme === UserTheme.LIGHT ?
        UserTheme.DARK :
      theme === UserTheme.DARK ?
        UserTheme.SYSTEM :
        UserTheme.LIGHT;

    setTheme(nextTheme);
  }, [setTheme, theme]);

  const getIcon = (className: string) => {
    const baseRules = getBaseRules(
        <MoonIcon className={className} />,
        <SunIcon className={className} />,
    );
    return select({
      ...baseRules,
      system: selectSystem({
        ...baseRules,
      }),
      default: <MoonIcon className={className} />,
    });
  };

  const getAction = () => {
    // 语义：点击按钮将切换到的那一态，与 toggle 的循环一一对应。
    const baseRules = getBaseRules(t('theme.switchToDark'), t('theme.switchToLight'));
    return select({
      ...baseRules,
      system: t('theme.system'),
      default: t('theme.switchToDark'),
    });
  };

  return {
    select,
    toggle,
    getIcon,
    getAction,
    selectSystem,
    getSystemTheme,
  };
}
