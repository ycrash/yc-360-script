public class MyClass {
    public static void main(String args[]) {
      int x=10;
      int y=25;
      int z=x+y;
      try{
          Thread.sleep(30*60*1000);
      }catch(Exception e){}

      System.out.println("Sum of x+y = " + z);
    }
}